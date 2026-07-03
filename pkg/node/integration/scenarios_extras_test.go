// Extra scenarios covering the path-coverage holes that
// the original S1-S12 + fuzz didn't hit. All of these
// test EXISTING Node API surface, in different ordering
// scenarios. Not new features.
//
// S13 SingleCreatorGroup          — alice creates a group
//                                  with 0 invitees, sends
//                                  to herself, leaves.
// S14 CreatorSelfDissolve         — 3 peers in a group, all
//                                  non-creators leave, then
//                                  the creator leaves →
//                                  group gone for everyone.
// S15 RenameAndRemark             — SetGroupName / SetGroup
//                                  Remark by creator +
//                                  non-creator-rejected.
// S16 WalkOutWalkBack_HistoryZero — bob leaves, re-accepted,
//                                  his chat.enc is empty
//                                  (the v1.1.2 design:
//                                  "leave and forget" —
//                                  intentional, see comment).

package integration_test

import (
	"strings"
	"testing"

	"github.com/weishengsuptp/innerlink/pkg/group"
	"github.com/weishengsuptp/innerlink/pkg/node"
)

// S13: single-creator group
//
// Alice creates a group with NO invitees. She can
// send messages to herself, list the group, leave it
// (self-dissolve, since she's alone + creator). The
// group is gone from her members.json after leave.
func TestScenario_SingleCreatorGroup(t *testing.T) {
	h := NewHarness(t, []string{"alice"})

	// Create with 0 invitees.
	gid, err := h.CreateGroupAction("alice", "lone", nil)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	// Alice should see the group.
	snap := h.Snapshot()
	if _, ok := snap.PerPeer["alice"].GroupDirs[gid]; !ok {
		t.Fatalf("alice doesn't see her own group %s", shortGroupID(gid))
	}
	AssertGroupMemberSet(t, snap, "alice", gid, "alice")

	// Alice sends a message to herself. (In production
	// the broadcast loop skips self, so the message
	// stays in her local chat.enc.)
	rawID, _ := group.ParseGroupID(gid)
	if err := h.Peer("alice").Node.SendGroupMessage(rawID, "hello me"); err != nil {
		t.Fatalf("SendGroupMessage: %v", err)
	}
	// Verify chat.enc was written.
	hist, err := h.Peer("alice").Node.HistoryGroup(gid)
	if err != nil {
		t.Fatalf("HistoryGroup: %v", err)
	}
	if len(hist) == 0 {
		t.Errorf("expected alice to have 1 message in her own group, got 0")
	}

	// Alice self-dissolves (creator + alone = self-dissolve).
	if err := h.LeaveGroupAction("alice", gid); err != nil {
		t.Fatalf("LeaveGroup (self-dissolve): %v", err)
	}
	snap = h.Snapshot()
	if _, ok := snap.PerPeer["alice"].GroupDirs[gid]; ok {
		t.Errorf("expected group %s to be gone after self-dissolve, still present", shortGroupID(gid))
	}
}

// S14: creator self-dissolve after everyone else leaves
//
// 3-peer group: alice (creator) + bob + carol. Both
// non-creators leave, then alice leaves. After alice's
// leave the group is gone for everyone (creator was
// the last member, so her leave is a self-dissolve).
func TestScenario_CreatorSelfDissolveAfterAllLeave(t *testing.T) {
	h := NewHarness(t, []string{"alice", "bob", "carol"})

	bobID, _ := h.ResolvePeerID("bob")
	carolID, _ := h.ResolvePeerID("carol")

	gid, err := h.CreateGroupAction("alice", "g1", []string{bobID, carolID})
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	invBob, _ := h.InviteAction("alice", gid, bobID)
	if err := h.AcceptInviteAction("bob", invBob, "alice"); err != nil {
		t.Fatalf("Accept bob: %v", err)
	}
	invCarol, _ := h.InviteAction("alice", gid, carolID)
	if err := h.AcceptInviteAction("carol", invCarol, "alice"); err != nil {
		t.Fatalf("Accept carol: %v", err)
	}

	// Bob leaves.
	if err := h.LeaveGroupAction("bob", gid); err != nil {
		t.Fatalf("bob leave: %v", err)
	}
	// Carol leaves.
	if err := h.LeaveGroupAction("carol", gid); err != nil {
		t.Fatalf("carol leave: %v", err)
	}
	// Now alice is alone. Her leave = self-dissolve.
	if err := h.LeaveGroupAction("alice", gid); err != nil {
		t.Fatalf("alice self-dissolve: %v", err)
	}

	// Group should be gone for everyone.
	snap := h.Snapshot()
	for _, name := range []string{"alice", "bob", "carol"} {
		if _, ok := snap.PerPeer[name].GroupDirs[gid]; ok {
			t.Errorf("peer %s still has group %s after creator self-dissolve",
				name, shortGroupID(gid))
		}
	}
}

// S15: rename + remark
//
// alice renames the group + adds a remark. The rename
// is broadcast to bob + carol. Non-creator attempts to
// rename are rejected.
func TestScenario_RenameAndRemark(t *testing.T) {
	h := NewHarness(t, []string{"alice", "bob", "carol"})

	bobID, _ := h.ResolvePeerID("bob")
	carolID, _ := h.ResolvePeerID("carol")

	gid, err := h.CreateGroupAction("alice", "original", []string{bobID, carolID})
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	invBob, _ := h.InviteAction("alice", gid, bobID)
	if err := h.AcceptInviteAction("bob", invBob, "alice"); err != nil {
		t.Fatalf("Accept bob: %v", err)
	}
	invCarol, _ := h.InviteAction("alice", gid, carolID)
	if err := h.AcceptInviteAction("carol", invCarol, "alice"); err != nil {
		t.Fatalf("Accept carol: %v", err)
	}

	// Alice renames.
	if _, err := h.SetGroupNameAction("alice", gid, "renamed"); err != nil {
		t.Fatalf("alice SetGroupName: %v", err)
	}
	// Alice sets a remark.
	if _, err := h.SetGroupRemarkAction("alice", gid, "the cool group"); err != nil {
		t.Fatalf("alice SetGroupRemark: %v", err)
	}

	// Push the meta update to bob + carol (the harness
	// can't auto-deliver the broadcast since we don't
	// have a real channel). ApplyMetaUpdate reads the
	// payload and updates local members.json.
	alice := h.Peer("alice")
	rawID, _ := group.ParseGroupID(gid)
	metaEnv := SynthesizeMetaEnvelope(alice, rawID, "renamed", "the cool group")
	for _, who := range []string{"bob", "carol"} {
		if err := h.Peer(who).Node.ApplyMetaUpdate(metaEnv, alice.PeerIDBytes()); err != nil {
			t.Fatalf("%s ApplyMetaUpdate: %v", who, err)
		}
	}

	// All 3 should see the new name in their local
	// ListGroups view.
	snap := h.Snapshot()
	for _, who := range []string{"alice", "bob", "carol"} {
		gs := snap.PerPeer[who].Groups
		var found *node.GroupInfo
		for i := range gs {
			if gs[i].GroupID == gid {
				found = &gs[i]
				break
			}
		}
		if found == nil {
			t.Errorf("peer %s missing group %s in ListGroups", who, shortGroupID(gid))
			continue
		}
		if found.GroupName != "renamed" {
			t.Errorf("peer %s: expected GroupName='renamed', got %q", who, found.GroupName)
		}
	}

	// Non-creator attempts to rename → rejected.
	if _, err := h.SetGroupNameAction("bob", gid, "hacked"); err == nil {
		t.Errorf("expected bob's rename to be rejected, got nil error")
	} else if !strings.Contains(err.Error(), "creator") {
		t.Errorf("expected 'creator' rejection, got: %v", err)
	}
}

// S16: walk out, walk back, history is 0
//
// bob is in g1 with 3 messages from alice. bob leaves
// (his local chat.enc gets deleted). alice re-invites
// bob, bob re-accepts. bob's HistoryGroup returns 0
// messages — this is the v1.1.2 "leave and forget"
// design, matching Signal/WhatsApp semantics. Per
// product decision (2026-07-03), this is intentional.
//
// This scenario DOCUMENTS the behavior — it's not a
// bug. The assertion is "history == 0"; if that ever
// changes (e.g. v1.2 syncs history on re-accept), the
// test will fail and force an explicit decision.
func TestScenario_WalkOutWalkBack_HistoryZero(t *testing.T) {
	h := NewHarness(t, []string{"alice", "bob"})

	bobID, _ := h.ResolvePeerID("bob")
	gid, err := h.CreateGroupAction("alice", "g1", []string{bobID})
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	inv, _ := h.InviteAction("alice", gid, bobID)
	if err := h.AcceptInviteAction("bob", inv, "alice"); err != nil {
		t.Fatalf("Accept bob: %v", err)
	}

	// Alice sends 3 messages + delivers them to bob
	// (the in-process harness can't auto-deliver via
	// the channel broadcast loop since stub channels
	// have nil ch). The "群已创建" system message is
	// appended at group creation, so the total is +1.
	rawID, _ := group.ParseGroupID(gid)
	for i := 0; i < 3; i++ {
		if err := h.Peer("alice").Node.SendGroupMessage(rawID, "msg"); err != nil {
			t.Fatalf("SendGroupMessage %d: %v", i, err)
		}
		if err := h.DeliverGroupMessageAction("alice", "bob", gid, "msg"); err != nil {
			t.Fatalf("Deliver msg %d: %v", i, err)
		}
	}

	// Bob should see 3 user messages + 1 system =
	// 4 total.
	hist, err := h.Peer("bob").Node.HistoryGroup(gid)
	if err != nil {
		t.Fatalf("bob HistoryGroup (pre-leave): %v", err)
	}
	userMsgs := 0
	for _, m := range hist {
		if m.Direction == "in" {
			userMsgs++
		}
	}
	if userMsgs != 3 {
		t.Errorf("expected bob to have 3 user messages pre-leave, got %d (total %d)", userMsgs, len(hist))
	}

	// Bob leaves.
	if err := h.LeaveGroupAction("bob", gid); err != nil {
		t.Fatalf("bob leave: %v", err)
	}

	// Bob's local chat.enc is gone.
	hist, err = h.Peer("bob").Node.HistoryGroup(gid)
	if err == nil && len(hist) != 0 {
		t.Errorf("expected bob's HistoryGroup to fail (group gone) or be empty post-leave, got %d msgs", len(hist))
	}

	// Alice re-invites bob, bob re-accepts.
	inv2, _ := h.InviteAction("alice", gid, bobID)
	if err := h.AcceptInviteAction("bob", inv2, "alice"); err != nil {
		t.Fatalf("bob re-accept: %v", err)
	}

	// Bob's user-message history is 0 — leave-and-forget
	// design. (The system "群已创建" message from the
	// second CreateGroup/Accept cycle may re-appear,
	// but the 3 user messages from before the leave
	// are gone.)
	hist, err = h.Peer("bob").Node.HistoryGroup(gid)
	if err != nil {
		t.Fatalf("bob HistoryGroup (post-reaccept): %v", err)
	}
	userMsgs = 0
	for _, m := range hist {
		if m.Direction == "in" {
			userMsgs++
		}
	}
	if userMsgs != 0 {
		t.Errorf("expected bob's user-message history to be 0 after walk-out-walk-back, got %d (this is a v1.1.2 design — if v1.2 changes this, update the test)", userMsgs)
	}
	// Alice's user-message history is still 3.
	hist, err = h.Peer("alice").Node.HistoryGroup(gid)
	if err != nil {
		t.Fatalf("alice HistoryGroup: %v", err)
	}
	aliceUserMsgs := 0
	for _, m := range hist {
		if m.Direction == "out" {
			aliceUserMsgs++
		}
	}
	if aliceUserMsgs != 3 {
		t.Errorf("expected alice's user-message history to still be 3, got %d", aliceUserMsgs)
	}
}
