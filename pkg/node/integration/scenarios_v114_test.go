// Scenarios S17-S19: post-v1.1.4 path coverage.
//
// S17 CrossGroupChatIsolation — alice is in g1 + g2,
//                               bob is in g1 only,
//                               carol is in g2 only.
//                               A message alice sends in
//                               g1 must NOT show up in
//                               carol's g2 chat.enc (and
//                               vice versa). Tests that
//                               the per-group chat log is
//                               keyed by GroupID and not
//                               by some global "messages"
//                               bucket that would leak
//                               across groups.
//
// S18 AliceTwoGroupsAtOnce   — alice creates g1 and g2
//                               back-to-back, sends a
//                               message in each, leaves
//                               g1, then asks g2 for
//                               ListGroups and
//                               HistoryGroup. Verifies
//                               the two groups are
//                               independent: leaving g1
//                               does not touch g2's
//                               members.json or chat.enc.
//
// S19 RestartHistoryReplay   — alice + bob in g1, alice
//                               sends 2 messages, bob
//                               delivers them. Then
//                               alice restarts. After
//                               restart, alice's
//                               HistoryGroup returns the
//                               2 messages (her OWN
//                               chat.enc on disk
//                               survives restart; only
//                               the in-memory queue is
//                               gone). This is the
//                               "i closed and reopened
//                               the app" smoke test.

package integration_test

import (
	"strings"
	"testing"
	"time"

	"github.com/weishengsuptp/innerlink/pkg/group"
	"github.com/weishengsuptp/innerlink/pkg/node"
)

func TestScenario_CrossGroupChatIsolation(t *testing.T) {
	h := NewHarness(t, []string{"alice", "bob", "carol"})

	bobID, _ := h.ResolvePeerID("bob")
	carolID, _ := h.ResolvePeerID("carol")

	// alice creates two groups with disjoint members.
	g1, err := h.CreateGroupAction("alice", "g1-alpha", []string{bobID})
	if err != nil {
		t.Fatalf("CreateGroup g1: %v", err)
	}
	g2, err := h.CreateGroupAction("alice", "g2-beta", []string{carolID})
	if err != nil {
		t.Fatalf("CreateGroup g2: %v", err)
	}
	if g1 == g2 {
		t.Fatalf("g1 and g2 have the same GroupID %s", shortGroupID(g1))
	}

	// bob accepts g1, carol accepts g2.
	invBob, _ := h.InviteAction("alice", g1, bobID)
	if err := h.AcceptInviteAction("bob", invBob, "alice"); err != nil {
		t.Fatalf("bob accept g1: %v", err)
	}
	invCarol, _ := h.InviteAction("alice", g2, carolID)
	if err := h.AcceptInviteAction("carol", invCarol, "alice"); err != nil {
		t.Fatalf("carol accept g2: %v", err)
	}

	// alice sends one message in each group.
	rawG1, _ := group.ParseGroupID(g1)
	rawG2, _ := group.ParseGroupID(g2)
	if err := h.Peer("alice").Node.SendGroupMessage(rawG1, "alpha-only-message"); err != nil {
		t.Fatalf("alice send g1: %v", err)
	}
	if err := h.Peer("alice").Node.SendGroupMessage(rawG2, "beta-only-message"); err != nil {
		t.Fatalf("alice send g2: %v", err)
	}
	// Deliver to the respective members.
	if err := h.DeliverGroupMessageAction("alice", "bob", g1, "alpha-only-message"); err != nil {
		t.Fatalf("deliver g1 -> bob: %v", err)
	}
	if err := h.DeliverGroupMessageAction("alice", "carol", g2, "beta-only-message"); err != nil {
		t.Fatalf("deliver g2 -> carol: %v", err)
	}

	// bob's g1 history should contain "alpha-only-message".
	histBob, err := h.Peer("bob").Node.HistoryGroup(g1)
	if err != nil {
		t.Fatalf("bob HistoryGroup(g1): %v", err)
	}
	hasAlpha := false
	for _, m := range histBob {
		if m.Body == "alpha-only-message" {
			hasAlpha = true
		}
		// Bob must NOT see the g2 message in g1.
		if m.Body == "beta-only-message" {
			t.Errorf("bob's g1 history leaked g2 message %q", m.Body)
		}
	}
	if !hasAlpha {
		t.Errorf("bob g1 history missing alpha-only-message; got %d msgs", len(histBob))
	}

	// carol's g2 history should contain "beta-only-message"
	// and NOT "alpha-only-message".
	histCarol, err := h.Peer("carol").Node.HistoryGroup(g2)
	if err != nil {
		t.Fatalf("carol HistoryGroup(g2): %v", err)
	}
	hasBeta := false
	for _, m := range histCarol {
		if m.Body == "beta-only-message" {
			hasBeta = true
		}
		if m.Body == "alpha-only-message" {
			t.Errorf("carol's g2 history leaked g1 message %q", m.Body)
		}
	}
	if !hasBeta {
		t.Errorf("carol g2 history missing beta-only-message; got %d msgs", len(histCarol))
	}

	// Belt-and-suspenders: bob's g2 history should not
	// exist (he's not a member), and carol's g1 history
	// should not exist (she's not a member).
	if hist, err := h.Peer("bob").Node.HistoryGroup(g2); err == nil && len(hist) > 0 {
		t.Errorf("bob (not in g2) has %d messages in g2 history", len(hist))
	}
	if hist, err := h.Peer("carol").Node.HistoryGroup(g1); err == nil && len(hist) > 0 {
		t.Errorf("carol (not in g1) has %d messages in g1 history", len(hist))
	}
}

func TestScenario_AliceTwoGroupsAtOnce(t *testing.T) {
	h := NewHarness(t, []string{"alice", "bob", "carol"})

	bobID, _ := h.ResolvePeerID("bob")
	carolID, _ := h.ResolvePeerID("carol")

	g1, err := h.CreateGroupAction("alice", "g1", []string{bobID})
	if err != nil {
		t.Fatalf("CreateGroup g1: %v", err)
	}
	g2, err := h.CreateGroupAction("alice", "g2", []string{carolID})
	if err != nil {
		t.Fatalf("CreateGroup g2: %v", err)
	}
	invBob, _ := h.InviteAction("alice", g1, bobID)
	if err := h.AcceptInviteAction("bob", invBob, "alice"); err != nil {
		t.Fatalf("bob accept g1: %v", err)
	}
	invCarol, _ := h.InviteAction("alice", g2, carolID)
	if err := h.AcceptInviteAction("carol", invCarol, "alice"); err != nil {
		t.Fatalf("carol accept g2: %v", err)
	}

	// alice sends one message in each.
	rawG1, _ := group.ParseGroupID(g1)
	rawG2, _ := group.ParseGroupID(g2)
	if err := h.Peer("alice").Node.SendGroupMessage(rawG1, "g1-text"); err != nil {
		t.Fatalf("alice send g1: %v", err)
	}
	if err := h.Peer("alice").Node.SendGroupMessage(rawG2, "g2-text"); err != nil {
		t.Fatalf("alice send g2: %v", err)
	}

	// alice leaves g1 (bob is non-creator, so this is
	// alice-the-creator's leave — but wait, the
	// creator-can't-leave rule applies. To test
	// independence, let bob leave g1, then verify
	// g2 is intact.)
	if err := h.LeaveGroupAction("bob", g1); err != nil {
		t.Fatalf("bob leave g1: %v", err)
	}

	// alice's ListGroups must still show g2 (with carol).
	snap := h.Snapshot()
	var g2info *node.GroupInfo
	for i := range snap.PerPeer["alice"].Groups {
		if snap.PerPeer["alice"].Groups[i].GroupID == g2 {
			g2info = &snap.PerPeer["alice"].Groups[i]
			break
		}
	}
	if g2info == nil {
		t.Fatalf("alice's ListGroups missing g2 after bob left g1")
	}
	if g2info.Creator != h.Peer("alice").PeerID() {
		t.Errorf("g2.Creator mismatch: got %s, want alice", shortPeerID(g2info.Creator))
	}
	if len(g2info.Members) != 2 {
		t.Errorf("g2.Members len = %d, want 2 (alice+carol)", len(g2info.Members))
	}

	// alice's g2 history must still have "g2-text" and
	// NOT "g1-text".
	hist, err := h.Peer("alice").Node.HistoryGroup(g2)
	if err != nil {
		t.Fatalf("alice HistoryGroup g2: %v", err)
	}
	hasG2 := false
	for _, m := range hist {
		if m.Body == "g2-text" {
			hasG2 = true
		}
		if m.Body == "g1-text" {
			t.Errorf("alice's g2 history leaked g1 message %q", m.Body)
		}
	}
	if !hasG2 {
		t.Errorf("alice g2 history missing g2-text; got %d msgs", len(hist))
	}

	// alice's g1 history — she's the creator and didn't
	// leave, so her local copy survives. (Bob's local
	// copy is gone, but the test focuses on alice.)
	hist1, err := h.Peer("alice").Node.HistoryGroup(g1)
	if err != nil {
		t.Fatalf("alice HistoryGroup g1: %v", err)
	}
	hasG1 := false
	for _, m := range hist1 {
		if m.Body == "g1-text" {
			hasG1 = true
		}
	}
	if !hasG1 {
		t.Errorf("alice g1 history missing g1-text; got %d msgs", len(hist1))
	}
}

func TestScenario_RestartHistoryReplay(t *testing.T) {
	h := NewHarness(t, []string{"alice", "bob"})

	bobID, _ := h.ResolvePeerID("bob")
	gid, err := h.CreateGroupAction("alice", "g1", []string{bobID})
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	inv, _ := h.InviteAction("alice", gid, bobID)
	if err := h.AcceptInviteAction("bob", inv, "alice"); err != nil {
		t.Fatalf("bob accept: %v", err)
	}

	rawID, _ := group.ParseGroupID(gid)
	if err := h.Peer("alice").Node.SendGroupMessage(rawID, "msg-1"); err != nil {
		t.Fatalf("alice send 1: %v", err)
	}
	if err := h.Peer("alice").Node.SendGroupMessage(rawID, "msg-2"); err != nil {
		t.Fatalf("alice send 2: %v", err)
	}
	if err := h.DeliverGroupMessageAction("alice", "bob", gid, "msg-1"); err != nil {
		t.Fatalf("deliver msg-1: %v", err)
	}
	if err := h.DeliverGroupMessageAction("alice", "bob", gid, "msg-2"); err != nil {
		t.Fatalf("deliver msg-2: %v", err)
	}

	// Pre-restart: alice has both messages in chat.enc.
	pre, err := h.Peer("alice").Node.HistoryGroup(gid)
	if err != nil {
		t.Fatalf("pre-restart HistoryGroup: %v", err)
	}
	if len(pre) < 2 {
		t.Fatalf("pre-restart alice has %d messages, want >= 2", len(pre))
	}
	// (The exact count may be 2 or 4 — the system
	// "群已创建" + outbound "msg-1" + outbound "msg-2" +
	// maybe a 群已创建 from bob's accept broadcast —
	// depends on whether the harness counts system
	// messages. We just check msg-1 and msg-2 are there.)

	// Alice restarts. The peer's DataDir is the same,
	// so device.key + members.json + chat.enc all
	// survive. The in-memory Node is gone, replaced by
	// a fresh one. chat.enc is re-read on startup.
	if err := h.RestartPeerAction("alice"); err != nil {
		t.Fatalf("alice restart: %v", err)
	}

	// Post-restart: alice's HistoryGroup must still
	// return both messages. This is the "i closed and
	// reopened the app and the chat is still there"
	// smoke test.
	post, err := h.Peer("alice").Node.HistoryGroup(gid)
	if err != nil {
		t.Fatalf("post-restart HistoryGroup: %v", err)
	}
	has1, has2 := false, false
	for _, m := range post {
		if m.Body == "msg-1" {
			has1 = true
		}
		if m.Body == "msg-2" {
			has2 = true
		}
	}
	if !has1 {
		t.Errorf("post-restart: missing msg-1; got %d messages", len(post))
	}
	if !has2 {
		t.Errorf("post-restart: missing msg-2; got %d messages", len(post))
	}
	// Belt: the post-restart count must equal the
	// pre-restart count. Nothing new should be added
	// by restart itself.
	if len(post) != len(pre) {
		t.Errorf("post-restart count %d != pre-restart count %d", len(post), len(pre))
	}
	// Belt 2: alice's GroupDirs still has g1 with the
	// same members.
	snap := h.Snapshot()
	mems, ok := snap.PerPeer["alice"].GroupDirs[gid]
	if !ok {
		t.Fatalf("post-restart: alice lost g1 from GroupDirs")
	}
	// alice is the creator + bob; members list should
	// still be {alice, bob}.
	if !contains(mems, h.Peer("alice").PeerID()) {
		t.Errorf("post-restart: alice's g1 missing self; members=%v", mems)
	}
	if !contains(mems, bobID) {
		t.Errorf("post-restart: alice's g1 missing bob; members=%v", mems)
	}
}

// TestScenario_StaleRosterRefused — v1.1.6 — exercises
// the LastModified freshness gate added to ApplyRosterUpdate.
//
// The bug this catches (real reproduction on the user's
// 3-machine setup, 2026-07-05 16:11-16:12 and again at
// 17:12-17:14): a peer that was offline during a leave
// holds a stale m.Members snapshot. On reconnect,
// syncRostersToPeer sends the stale snapshot to peers who
// already converged. Pre-v1.1.6, ApplyRosterUpdate blindly
// overwrote local m with the inbound — undoing the
// converged state on every offline-peer reconnect.
//
// v1.1.6 fixes this by carrying m.LastModified in the
// rosterPayload. A strictly older inbound is refused, so
// a re-connecting peer with stale state cannot drag the
// receiver back. This test is the in-process proof.
func TestScenario_StaleRosterRefused(t *testing.T) {
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

	// Step 1: alice.LoadMembers so we can manipulate the
	// on-disk local timestamp to anchor the "this is
	// newer than any plausible inbound" baseline.
	rawID, _ := group.ParseGroupID(gid)
	localBefore, err := group.LoadMembers(h.Peer("alice").DataDir, rawID)
	if err != nil {
		t.Fatalf("alice load local: %v", err)
	}

	// Step 2: synthesize a STALE roster update from a peer
	// that doesn't even include alice's own id in the
	// (purportedly snapshotted) Members list — mirroring
	// the production bug where a re-connecting peer holds
	// pre-leave state and pushes it on the next handshake.
	//
	// Payload's LastModified is EXPLICITLY OLDER than
	// alice's local. The freshness gate must refuse.
	staleLM := localBefore.LastModified.Add(-1 * time.Hour)
	stalePayload := SynthesizeRosterEnvelopeWithLastModified(
		h.Peer("bob"),
		gid,
		localBefore,
		staleLM,
	)
	if err := h.Peer("alice").Node.ApplyRosterUpdate(stalePayload, h.Peer("bob").PeerIDBytes()); err != nil {
		t.Fatalf("ApplyRosterUpdate(stale) returned err: %v", err)
	}

	// alice's local MUST still match the prior (converged)
	// state — the stale inbound was refused, NOT written.
	localAfterStale, err := group.LoadMembers(h.Peer("alice").DataDir, rawID)
	if err != nil {
		t.Fatalf("alice load after stale push: %v", err)
	}
	if !localAfterStale.LastModified.Equal(localBefore.LastModified) {
		t.Errorf("alice local LastModified bumped by stale push: before=%v after=%v",
			localBefore.LastModified.Format("15:04:05.000"), localAfterStale.LastModified.Format("15:04:05.000"))
	}
	for _, want := range []string{
		h.Peer("alice").PeerID(),
		bobID,
		carolID,
	} {
		if !localAfterStale.Contains(want) {
			t.Errorf("alice local after stale push missing %s; members=%v",
				want[:8], memberPIDs(localAfterStale.Members))
		}
	}

	// Step 3: now synthesize a FRESH inbound (LastModified
	// strictly after local) and verify it IS accepted —
	// the gate is bidirectional. This proves the
	// freshness check isn't a one-way "always refuse"
	// blanket that would break legit propagation
	// (TestScenario_AcceptWhileOffline, etc.).
	freshLM := localBefore.LastModified.Add(1 * time.Hour)
	freshPayload := SynthesizeRosterEnvelopeWithLastModified(
		h.Peer("carol"),
		gid,
		localBefore,
		freshLM,
	)
	if err := h.Peer("alice").Node.ApplyRosterUpdate(freshPayload, h.Peer("carol").PeerIDBytes()); err != nil {
		t.Fatalf("ApplyRosterUpdate(fresh) returned err: %v", err)
	}
	localAfterFresh, err := group.LoadMembers(h.Peer("alice").DataDir, rawID)
	if err != nil {
		t.Fatalf("alice load after fresh push: %v", err)
	}
	if !localAfterFresh.LastModified.After(localBefore.LastModified) {
		t.Errorf("alice local LastModified did NOT bump on fresh push: before=%v after=%v",
			localBefore.LastModified.Format("15:04:05.000"), localAfterFresh.LastModified.Format("15:04:05.000"))
	}

	// Step 4: backlevel (pre-v1.1.6) inbound — sender carries
	// no LastModified (rp.LastModified == zero). With local
	// LM > 0 (we have v1.1.6 stamp), the gate treats this as
	// "sender has no freshness info" and REFUSES by default.
	// This is the strictest compatibility choice; it forces
	// a "wait for next handshake" path, which is fine
	// because both sides will be on v1.1.6 after the
	// upgrade window.
	stampBeforeBacklevel := localAfterFresh.LastModified
	backlevelPayload := SynthesizeRosterEnvelopeWithLastModified(
		h.Peer("bob"),
		gid,
		localBefore,
		time.Time{}, // zero = pre-LM era sender
	)
	if err := h.Peer("alice").Node.ApplyRosterUpdate(backlevelPayload, h.Peer("bob").PeerIDBytes()); err != nil {
		t.Fatalf("ApplyRosterUpdate(backlevel) returned err: %v", err)
	}
	localAfterBacklevel, err := group.LoadMembers(h.Peer("alice").DataDir, rawID)
	if err != nil {
		t.Fatalf("alice load after backlevel push: %v", err)
	}
	if !localAfterBacklevel.LastModified.Equal(stampBeforeBacklevel) {
		t.Errorf("backlevel inbound (zero LM) overwrote alice's v1.1.6+ local LM: before=%v after=%v (would drag local back to pre-LM era)",
			stampBeforeBacklevel.Format("15:04:05.000"),
			localAfterBacklevel.LastModified.Format("15:04:05.000"))
	}
}

// memberPIDs returns the PeerIDs of m.Members as short hex
// for failure messages — keeps the diagnostic output small
// without pulling the full Member struct into t.Errorf args.
func memberPIDs(ms []group.Member) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		if len(m.PeerID) >= 8 {
			out = append(out, m.PeerID[:8])
		} else {
			out = append(out, m.PeerID)
		}
	}
	return out
}

// aliceGroup is a local alias for *node.GroupInfo used
// by S18's local variable — keeps the test body short
// without polluting the package.

// contains returns true if s is in xs. Tiny helper.
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// silence "imported and not used" if a future refactor
// drops the strings import; current code does use it via
// the body of helpers above (none today, but the
// scenario tests reference strings.Contains-style
// patterns elsewhere — keep the import for now).
var _ = strings.Contains
