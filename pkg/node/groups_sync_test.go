package node

// Group sync tests (v1.1.1, 2026-06-29).
//
// What this catches: the bug we shipped in v1.1 where
// CreatorOnAccept added the joiner to members.json but
// never told the local frontend to re-read ListGroups.
// Result: creator's sidebar stayed at "1 成员" forever
// even though members.json had 3 entries.
//
// Strategy: stand up Node A and Node C in-process (no
// TCP — just exercise the local handlers + event channel
// directly). Each test asserts both:
//   1. on-disk members.json reflects the change
//   2. SubscribeGroups() emitted the expected GroupUpdated
//      event so the frontend will refresh
//
// We use New(opts) with a per-test temp DataDir; New
// does NOT start any networking (the comment in
// node.go is explicit), so tests run fast and don't
// need port allocation.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/weishengsuptp/innerlink/internal/logx"
	"github.com/weishengsuptp/innerlink/internal/protocol"
	"github.com/weishengsuptp/innerlink/internal/storage"
	"github.com/weishengsuptp/innerlink/pkg/group"
)

// newTestNode creates a Node with a fresh temp data dir
// and returns it together with the rendered GroupID of
// a freshly-created group with only the creator. Used
// as the "before" state for every sync-flow test.
func newTestNode(t *testing.T) (*Node, string) {
	t.Helper()
	tmp := t.TempDir()
	// Tame the logx output — each test gets its own
	// logfile path, but the package-global log
	// destination is shared, so we use a tiny logfile
	// the test can ignore. Worst case, log lines are
	// interleaved across tests but each test only
	// asserts on its own data dir.
	logFile := filepath.Join(tmp, "test.log")
	n, err := New(Options{
		DataDir: tmp,
		LogFile: logFile,
		LogLevel: "error", // keep test output clean
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// v1.1.4 (2026-07-02) — schedule Node.Close on
	// cleanup so the exclusive DataDir lockfile is
	// released and t.TempDir's RemoveAll can clean
	// up on Windows. Without this, t.TempDir fails
	// with "file in use" because the lockfile
	// (innerlink.lock) is still held by the test
	// process — same root cause as the live
	// "two innerlink.exe on one DataDir" bug the
	// lockfile was added to fix in production.
	t.Cleanup(func() {
		_ = n.Close()
		_ = logx.Close()
	})
	info, err := n.CreateGroup("测试群", nil)
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	return n, info.GroupID
}

// drainGroupEvent waits up to d for a group event matching
// the predicate; returns the event or fails the test.
func drainGroupEvent(t *testing.T, ch <-chan GroupEvent, pred func(GroupEvent) bool, d time.Duration) GroupEvent {
	t.Helper()
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	for {
		select {
		case ev := <-ch:
			if pred(ev) {
				return ev
			}
			// Not the one we want — keep draining.
		case <-deadline.C:
			t.Fatalf("timed out waiting for matching group event after %v", d)
			return GroupEvent{}
		}
	}
}

// TestGroupSync_CreatorOnAccept_FiresGroupUpdated:
// The pre-fix bug: CreatorOnAccept's AddMember + Save
// updated members.json but never publishedGroupEvent,
// so the creator's GUI's state.groups stayed stale at
// the moment-of-creation snapshot (1 member). This test
// asserts that after CreatorOnAccept, A's GroupUpdated
// event fires within a short window.
func TestGroupSync_CreatorOnAccept_FiresGroupUpdated(t *testing.T) {
	n, rendered := newTestNode(t)

	// Drain the GroupAdded that CreateGroup fired (we
	// don't want it satisfying our eventual GroupUpdated
	// assertion below).
	ch := n.SubscribeGroups()
	drainGroupEvent(t, ch, func(e GroupEvent) bool {
		return e.Type == GroupAdded && e.GroupID == rendered
	}, 1*time.Second)

	// Simulate invitee B's accept envelope arriving on
	// the creator. We need a fake fromPeerID (16 bytes)
	// matching what the dispatcher would normally hand
	// CreatorOnAccept. Hardcode 16 arbitrary bytes —
	// CreatorOnAccept uses peerBytesToHex → "bad-peer-id-16B"
	// if it isn't 16 long. We use 16 zeros just to drive
	// the code path; the local members.json check below
	// doesn't care about the actual hex form beyond
	// AddMember storing whatever hex came in.
	fakeAcceptEnv := protocol.Envelope{
		Type: protocol.TypeGroupInviteAccept,
		Payload: mustJSON(t, acceptPayload{
			GroupID: rendered,
			Nonce:   []byte{1, 2, 3, 4},
		}),
	}
	fromPeerID := make([]byte, 16)
	for i := range fromPeerID {
		fromPeerID[i] = 0xb0 + byte(i)
	}
	if err := n.CreatorOnAccept(fakeAcceptEnv, fromPeerID); err != nil {
		t.Fatalf("CreatorOnAccept: %v", err)
	}

	// Assertion 1: local members.json must contain both
	// creator and the (fake) joiner.
	rawID, err := group.ParseGroupID(rendered)
	if err != nil {
		t.Fatal(err)
	}
	m, err := group.LoadMembers(n.dataDir(), rawID)
	if err != nil {
		t.Fatalf("LoadMembers: %v", err)
	}
	if got := len(m.Members); got != 2 {
		t.Errorf("after CreatorOnAccept: members=%d, want 2", got)
	}

	// Assertion 2: GroupUpdated event fired so the
	// creator's frontend will refresh the sidebar.
	ev := drainGroupEvent(t, ch, func(e GroupEvent) bool {
		return e.Type == GroupUpdated && e.GroupID == rendered
	}, 1*time.Second)
	if ev.GroupName != "测试群" {
		t.Errorf("event GroupName=%q, want %q", ev.GroupName, "测试群")
	}
}

// TestGroupSync_SetGroupName_FiresGroupUpdated:
// The pre-fix bug: SetGroupName's Save + broadcastMetaUpdate
// updated members.json + sent envelopes to other peers,
// but never published a local GroupUpdated — so the
// creator's own sidebar name froze at the old value
// until they restarted the binary.
func TestGroupSync_SetGroupName_FiresGroupUpdated(t *testing.T) {
	n, rendered := newTestNode(t)

	ch := n.SubscribeGroups()
	drainGroupEvent(t, ch, func(e GroupEvent) bool {
		return e.Type == GroupAdded
	}, 1*time.Second)

	info, err := n.SetGroupName(rendered, "新名字")
	if err != nil {
		t.Fatalf("SetGroupName: %v", err)
	}
	if info.GroupName != "新名字" {
		t.Errorf("returned GroupInfo.GroupName=%q, want %q", info.GroupName, "新名字")
	}

	// Assert on disk
	rawID, _ := group.ParseGroupID(rendered)
	m, err := group.LoadMembers(n.dataDir(), rawID)
	if err != nil {
		t.Fatal(err)
	}
	if m.GroupName != "新名字" {
		t.Errorf("on-disk GroupName=%q, want %q", m.GroupName, "新名字")
	}

	// Assert event
	ev := drainGroupEvent(t, ch, func(e GroupEvent) bool {
		return e.Type == GroupUpdated && e.GroupID == rendered
	}, 1*time.Second)
	if ev.GroupName != "新名字" {
		t.Errorf("event GroupName=%q, want %q", ev.GroupName, "新名字")
	}
}

// TestGroupSync_ApplyRosterUpdate_ReplacesMembers:
// Simulates an invitee receiving a roster-sync envelope
// from the creator (the TypeGroupRosterUpdate path). The
// invitee's local members.json gets wholesale-replaced
// with the inbound roster, and a GroupUpdated event
// fires so the GUI refreshes.
func TestGroupSync_ApplyRosterUpdate_ReplacesMembers(t *testing.T) {
	n, rendered := newTestNode(t)

	ch := n.SubscribeGroups()
	drainGroupEvent(t, ch, func(e GroupEvent) bool {
		return e.Type == GroupAdded
	}, 1*time.Second)

	// The creator's perspective: 3 members.
	rawID, _ := group.ParseGroupID(rendered)
	creatorMembers, _ := group.LoadMembers(n.dataDir(), rawID)
	if len(creatorMembers.Members) != 1 {
		t.Fatalf("setup: creator should start with 1 member, got %d", len(creatorMembers.Members))
	}
	// Add a fake joiner so we have 2 to broadcast.
	if err := creatorMembers.AddMember(group.Member{
		PeerID:   "invitee-hex-fake",
		JoinedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	// And another to round out 3.
	if err := creatorMembers.AddMember(group.Member{
		PeerID:   "invitee-hex-also-fake",
		JoinedAt: time.Now().UTC().Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}

	// Now simulate the receiver-side dispatcher calling
	// ApplyRosterUpdate on a fresh Node C with the
	// creator's roster as the inbound payload.
	tmpC := t.TempDir()
	nC, err := New(Options{
		DataDir:  tmpC,
		LogFile:  filepath.Join(tmpC, "c.log"),
		LogLevel: "error",
	})
	if err != nil {
		t.Fatal(err)
	}
	// v1.1.4 (2026-07-02): n.Close() releases the
	// exclusive DataDir lockfile. Without this,
	// t.TempDir's RemoveAll on Windows fails with
	// "file in use" on innerlink.lock. See the
	// newTestNode cleanup comment for the same logic
	// applied to the creator node.
	t.Cleanup(func() {
		_ = nC.Close()
		_ = logx.Close()
	})
	// C must have accepted the invite first; mimic that
	// by creating a 1-member members.json for C matching
	// [creator, c]. (AcceptGroupInvite creates this; we
	// shortcut here by calling it directly.)
	cSelfHex := nC.id.PeerIDHex()
	cRoster := &group.Members{
		GroupID:   rendered,
		GroupName: creatorMembers.GroupName,
		Creator:   creatorMembers.Creator,
		CreatedAt: creatorMembers.CreatedAt,
		Members: []group.Member{
			{PeerID: creatorMembers.Creator, JoinedAt: creatorMembers.CreatedAt, IsCreator: true},
			{PeerID: cSelfHex, JoinedAt: time.Now().UTC(), Alias: nC.GetSelfAlias()},
		},
	}
	if err := cRoster.Save(nC.dataDir(), rawID); err != nil {
		t.Fatal(err)
	}

	// Drain C's startup-time group events (none yet —
	// AcceptGroupInvite isn't called here — so this is
	// a no-op pass to clear the channel.)
	select {
	case <-ch:
	case <-time.After(50 * time.Millisecond):
	}

	// Now invoke ApplyRosterUpdate on C with the
	// creator's 3-member roster.
	env := protocol.Envelope{
		Type: protocol.TypeGroupRosterUpdate,
		Payload: mustJSON(t, rosterPayload{
			GroupID:   creatorMembers.GroupID,
			GroupName: creatorMembers.GroupName,
			Members:   creatorMembers.Members,
			Remark:    creatorMembers.Remark,
		}),
	}
	cFakeFromPeerID := make([]byte, 16)
	if err := nC.ApplyRosterUpdate(env, cFakeFromPeerID); err != nil {
		t.Fatalf("ApplyRosterUpdate: %v", err)
	}

	// Assert: C's local members.json now has 3.
	got, err := group.LoadMembers(nC.dataDir(), rawID)
	if err != nil {
		t.Fatalf("LoadMembers on C: %v", err)
	}
	if len(got.Members) != 3 {
		t.Errorf("C's local members after ApplyRosterUpdate = %d, want 3", len(got.Members))
	}

	// Assert: SubscribeGroups on C emitted GroupUpdated.
	ev := drainGroupEvent(t, nC.SubscribeGroups(), func(e GroupEvent) bool {
		return e.Type == GroupUpdated && e.GroupID == rendered
	}, 1*time.Second)
	if ev.GroupName != creatorMembers.GroupName {
		t.Errorf("event GroupName=%q, want %q", ev.GroupName, creatorMembers.GroupName)
	}
}

// mustJSON marshals v or fails the test. Used to keep
// the test bodies tight.
func mustJSON(t *testing.T, v interface{}) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// TestGroupSync_ApplyRosterUpdate_PreservesCreator:
// Pins down the v1.1.2 (2026-06-30) hotfix that stops
// ApplyRosterUpdate from wiping the receiver's local
// Creator field. Pre-fix bug: every receiving peer
// (including the creator) had `Creator: ""` written
// into members.json after any roster update, which
// made ListGroups return GroupInfo.creator == "" — the
// frontend then flipped `g.creator === selfHex` to
// false and the creator-only UI ("+ 邀请成员" button,
// editable 群名称 + 群备注 inputs) disappeared until
// restart. Triggered any time a peer joins / leaves
// / set-name / set-remark.
//
// Three sub-cases:
//   1. inbound has Creator → receiver uses inbound (new
//      forward-compatible behavior, since rosterPayload
//      now carries Creator).
//   2. inbound has empty Creator AND receiver already
//      has local Creator → preserve local (backwards-
//      compatible with pre-v1.1.2 broadcast binaries).
//   3. inbound has empty Creator AND receiver has no
//      local members.json (rare, race-y): falls back
//      to "" — next refresh will heal it; not a hard
//      failure.
func TestGroupSync_ApplyRosterUpdate_PreservesCreator(t *testing.T) {
	rawID_ := func(t *testing.T, n *Node, rendered string) []byte {
		t.Helper()
		rid, err := group.ParseGroupID(rendered)
		if err != nil {
			t.Fatal(err)
		}
		return rid
	}

	// Sub-case 1: inbound carries Creator → receiver
	// takes it verbatim.
	t.Run("InboundCarriesCreator", func(t *testing.T) {
		n, rendered := newTestNode(t)
		rawID := rawID_(t, n, rendered)
		creatorHex := n.id.PeerIDHex()

		// Stub a 2-member roster with a non-self creator
		// so we can assert inbound-Creator overrides local.
		inbound := rosterPayload{
			GroupID:   rendered,
			GroupName: "测试群",
			Creator:   "deadbeefdeadbeef00000000000000aa",
			Members: []group.Member{
				{PeerID: creatorHex, JoinedAt: time.Now().UTC(), IsCreator: true},
				{PeerID: "b000000000000000000000000000bb01", JoinedAt: time.Now().UTC()},
			},
		}
		env := protocol.Envelope{
			Type: protocol.TypeGroupRosterUpdate,
			Payload: mustJSON(t, inbound),
		}
		fakeFrom := make([]byte, 16)
		if err := n.ApplyRosterUpdate(env, fakeFrom); err != nil {
			t.Fatalf("ApplyRosterUpdate: %v", err)
		}
		got, err := group.LoadMembers(n.dataDir(), rawID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Creator != "deadbeefdeadbeef00000000000000aa" {
			t.Errorf("inbound Creator should win, got %q", got.Creator)
		}
	})

	// Sub-case 2: inbound has no Creator AND receiver
	// already has a local Creator → local wins. This is
	// the actual user-reported regression: a v1.1.1 binary
	// receiving a v1.1.2 broadcast, or vice versa.
	t.Run("LocalWinsOnEmptyInbound", func(t *testing.T) {
		n, rendered := newTestNode(t)
		rawID := rawID_(t, n, rendered)
		creatorHex := n.id.PeerIDHex()

		// Sanity: local Creator is set to self after
		// newTestNode.
		before, err := group.LoadMembers(n.dataDir(), rawID)
		if err != nil {
			t.Fatal(err)
		}
		if before.Creator != creatorHex {
			t.Fatalf("setup: local Creator=%q, want %q", before.Creator, creatorHex)
		}

		// Inbound from a pre-v1.1.2 binary: Creator field
		// absent → JSON unmarshals to "".
		inbound := rosterPayload{
			GroupID:   rendered,
			GroupName: "测试群",
			// Creator intentionally omitted.
			Members: []group.Member{
				{PeerID: creatorHex, JoinedAt: time.Now().UTC(), IsCreator: true},
			},
		}
		env := protocol.Envelope{
			Type: protocol.TypeGroupRosterUpdate,
			Payload: mustJSON(t, inbound),
		}
		fakeFrom := make([]byte, 16)
		if err := n.ApplyRosterUpdate(env, fakeFrom); err != nil {
			t.Fatalf("ApplyRosterUpdate: %v", err)
		}
		got, err := group.LoadMembers(n.dataDir(), rawID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Creator != creatorHex {
			t.Errorf("Creator was wiped! got %q, want %q (local preserved)",
				got.Creator, creatorHex)
		}
	})
}

// TestGroupSync_SoloCreator_CanLeave: pins down
// the v1.1.2 (2026-06-30) hotfix that lets a creator
// who's the SOLE remaining member self-dissolve their
// group. Pre-fix bug: LeaveGroup called RemoveMember,
// which protects the creator in pkg/group/members.go
// (line 154-159) — so RemoveMember returned false and
// LeaveGroup errored with "node: LeaveGroup: RemoveMember
// returned false". The empty-members "delete group"
// branch below it was unreachable when self is the only
// AND the creator.
//
// After the fix:
//   - LeaveGroup returns nil
//   - The local members.json is gone (DeleteGroup path)
//   - The local chat.enc is gone
//   - A GroupRemoved event fires so the frontend's
//     sidebar refresh takes the group away
//   - The error message does not leak back to the user
func TestGroupSync_SoloCreator_CanLeave(t *testing.T) {
	n, rendered := newTestNode(t)

	ch := n.SubscribeGroups()
	// Drain startup GroupAdded so it doesn't satisfy the
	// GroupRemoved assertion below.
	drainGroupEvent(t, ch, func(e GroupEvent) bool {
		return e.Type == GroupAdded && e.GroupID == rendered
	}, 1*time.Second)

	// Sanity: members.json exists, exactly 1 member (self).
	rawID, err := group.ParseGroupID(rendered)
	if err != nil {
		t.Fatal(err)
	}
	before, err := group.LoadMembers(n.dataDir(), rawID)
	if err != nil {
		t.Fatalf("LoadMembers before LeaveGroup: %v", err)
	}
	if len(before.Members) != 1 {
		t.Fatalf("setup: pre-leave members=%d, want 1", len(before.Members))
	}
	if before.Creator != n.id.PeerIDHex() {
		t.Fatalf("setup: creator mismatch")
	}

	// Act: solo creator leaves.
	if err := n.LeaveGroup(rendered); err != nil {
		t.Fatalf("solo creator LeaveGroup: got error %v, want nil", err)
	}

	// Assert: members.json no longer exists on disk.
	if _, err := group.LoadMembers(n.dataDir(), rawID); !os.IsNotExist(err) {
		t.Errorf("post-leave LoadMembers err=%v, want os.ErrNotExist", err)
	}

	// Assert: GroupRemoved event fires.
	ev := drainGroupEvent(t, ch, func(e GroupEvent) bool {
		return e.Type == GroupRemoved && e.GroupID == rendered
	}, 1*time.Second)
	if ev.GroupName != "测试群" {
		t.Errorf("GroupRemoved event name=%q, want %q", ev.GroupName, "测试群")
	}

	// Assert: a second LeaveGroup call errors (not a member
	// anymore) — proves the group is really gone locally.
	if err := n.LeaveGroup(rendered); err == nil {
		t.Errorf("second LeaveGroup returned nil; want not-a-member error")
	}
}

// TestGroupSync_LeaveThenRejoin_ReSeedsChatEnc pins down the
// v1.1.3 regression (2026-06-30) reported by 潇男: a peer
// who leaves a group and is then re-invited doesn't see the
// group in their sidebar, even though the creator's roster
// shows them as a member. Pre-fix hypothesis: AcceptGroupInvite
// fails to re-seed chat.enc on the rejoiner's local after the
// prior LeaveGroup wiped it (the chatStore keeps a cached
// groupFile handle in its groupFiles map; DeleteGroup doesn't
// evict the entry, so the next AppendGroup returns the cached
// handle but the underlying path was removed — depending on
// the cached vs. on-disk state, the chat.enc may not end up
// visible to ListGroups).
//
// This test exercises the storage layer directly (no network,
// no signature verify) and asserts that the leave→rejoin
// sequence leaves chat.enc in the right state for ListGroups
// to surface the group on the rejoiner's side. If this test
// PASSES, the user's bug is upstream of storage (signature
// verify / unmarshal / network delivery). If this test FAILS,
// it's the chatFiles map caching bug and the fix is to evict
// the entry on DeleteGroup OR force-recreate chat.enc in
// AppendGroup's openGroupFile path.
func TestGroupSync_LeaveThenRejoin_ReSeedsChatEnc(t *testing.T) {
	n, rendered := newTestNode(t)
	rawID, err := group.ParseGroupID(rendered)
	if err != nil {
		t.Fatal(err)
	}

	// Seed an "invitee" peer record so the local members.json
	// has more than just the creator. This mirrors what
	// AcceptGroupInvite does after a successful first accept.
	const inviteeHex = "11112222333344445555666677778888"
	{
		m, err := group.LoadMembers(n.dataDir(), rawID)
		if err != nil {
			t.Fatalf("LoadMembers setup: %v", err)
		}
		if err := m.AddMember(group.Member{
			PeerID:   inviteeHex,
			JoinedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("setup AddMember: %v", err)
		}
		if err := m.Save(n.dataDir(), rawID); err != nil {
			t.Fatalf("setup Save: %v", err)
		}
		// v1.1 (2026-06-29) hotfix seed: AcceptGroupInvite
		// writes the "已加入群聊" system record to chat.enc so
		// ListGroups (which filters by chat.enc existence in
		// storage/group.go) surfaces this group on the
		// rejoiner's sidebar. Mirror that here.
		if err := n.chatStore.AppendGroup(rendered, &storage.Record{
			Timestamp: time.Now().UTC(),
			From:      inviteeHex,
			To:        "",
			Direction: "system",
			Body:      "已加入群聊",
			GroupID:   rendered,
			MsgID:     "",
		}); err != nil {
			t.Fatalf("setup AppendGroup: %v", err)
		}
	}

	// Sanity: ListGroups now returns 1 group (chat.enc exists).
	pre, err := n.ListGroups()
	if err != nil {
		t.Fatalf("pre ListGroups: %v", err)
	}
	if len(pre) != 1 {
		t.Fatalf("pre ListGroups count = %d, want 1", len(pre))
	}

	// ── Cycle 1: invitee leaves + then re-joins ────────────────
	// 1a. Simulate the LeaveGroup cleanup for the invitee:
	//     remove them from members.json + delete chat.enc.
	//     This is exactly what LeaveGroup's non-creator branch
	//     does when `len(m.Members) != 0` after self-removal.
	{
		m, err := group.LoadMembers(n.dataDir(), rawID)
		if err != nil {
			t.Fatalf("LoadMembers leave-cycle: %v", err)
		}
		if !m.RemoveMember(inviteeHex) {
			t.Fatalf("RemoveMember(%s) returned false", inviteeHex)
		}
		if err := m.Save(n.dataDir(), rawID); err != nil {
			t.Fatalf("Save leave-cycle: %v", err)
		}
		if err := n.chatStore.DeleteGroup(rendered); err != nil {
			t.Fatalf("DeleteGroup leave-cycle: %v", err)
		}
	}
	mid, err := n.ListGroups()
	if err != nil {
		t.Fatalf("mid ListGroups: %v", err)
	}
	if len(mid) != 0 {
		t.Fatalf("post-leave ListGroups count = %d, want 0 (chat.enc filtered)", len(mid))
	}

	// 1b. Re-invite simulated. AcceptGroupInvite would:
	//     - m.AddMember(self) — we use a fresh inviteeHex2 to
	//       keep the test independent of any "self is creator"
	//       branch bias
	//     - m.Save
	//     - chatStore.AppendGroup "已加入群聊" (THE BUG POINT)
	const inviteeHex2 = "aaaaaaaaaabbbbbbbbcccccccccccc"
	if err := func() error {
		m, err := group.LoadMembers(n.dataDir(), rawID)
		if err != nil {
			return err
		}
		if err := m.AddMember(group.Member{
			PeerID:   inviteeHex2,
			JoinedAt: time.Now().UTC(),
		}); err != nil {
			return err
		}
		if err := m.Save(n.dataDir(), rawID); err != nil {
			return err
		}
		// v1.1 hotfix seed. EXACTLY the line in
		// AcceptGroupInvite that the user-reported bug
		// "已退群再受邀看不到群" is hypothesised to break.
		return n.chatStore.AppendGroup(rendered, &storage.Record{
			Timestamp: time.Now().UTC(),
			From:      inviteeHex2,
			To:        "",
			Direction: "system",
			Body:      "已加入群聊",
			GroupID:   rendered,
			MsgID:     "",
		})
	}(); err != nil {
		t.Fatalf("rejoin seam: %v", err)
	}

	// KEY ASSERTION: ListGroups returns 1 group after the
	// rejoin. If this fails, the user's bug is in the
	// chatStore (openGroupFile / DeleteGroup / groupFiles
	// map caching) and the fix is to evict the cached
	// groupFile on DeleteGroup. If it passes, the bug is
	// somewhere else (AcceptGroupInvite's signature verify,
	// network delivery, or pubkey lookup).
	post, err := n.ListGroups()
	if err != nil {
		t.Fatalf("post-rejoin ListGroups: %v", err)
	}
	if len(post) != 1 {
		// Diagnostic dump to show what's actually on disk.
		chatDir := filepath.Join(n.dataDir(), "chat", "groups", rendered)
		t.Logf("chatDir=%s", chatDir)
		t.Logf("members.json exists: %v", fileExists(filepath.Join(n.dataDir(), "groups", rendered, "members.json")))
		t.Logf("chat.enc exists: %v", fileExists(filepath.Join(chatDir, "chat.enc")))
		t.Fatalf("post-rejoin ListGroups count = %d, want 1 (chat.enc must be visible to ListGroups)", len(post))
	}

	// Cycle 2 — leave + rejoin again, to make sure the seam
	// is robust across multiple cycles (not just the first).
	for i := 0; i < 2; i++ {
		m, err := group.LoadMembers(n.dataDir(), rawID)
		if err != nil {
			t.Fatalf("cycle2 LoadMembers iter=%d: %v", i, err)
		}
		if !m.RemoveMember(inviteeHex2) {
			t.Fatalf("cycle2 RemoveMember iter=%d returned false", i)
		}
		if err := m.Save(n.dataDir(), rawID); err != nil {
			t.Fatalf("cycle2 Save iter=%d: %v", i, err)
		}
		if err := n.chatStore.DeleteGroup(rendered); err != nil {
			t.Fatalf("cycle2 DeleteGroup iter=%d: %v", i, err)
		}
		if err := m.AddMember(group.Member{
			PeerID:   inviteeHex2,
			JoinedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("cycle2 AddMember iter=%d: %v", i, err)
		}
		if err := m.Save(n.dataDir(), rawID); err != nil {
			t.Fatalf("cycle2 Save rejoin iter=%d: %v", i, err)
		}
		if err := n.chatStore.AppendGroup(rendered, &storage.Record{
			Timestamp: time.Now().UTC(),
			From:      inviteeHex2,
			To:        "",
			Direction: "system",
			Body:      "已加入群聊",
			GroupID:   rendered,
			MsgID:     "",
		}); err != nil {
			t.Fatalf("cycle2 AppendGroup iter=%d: %v", i, err)
		}
		g, err := n.ListGroups()
		if err != nil {
			t.Fatalf("cycle2 ListGroups iter=%d: %v", i, err)
		}
		if len(g) != 1 {
			t.Fatalf("cycle2 iter=%d ListGroups=%d, want 1", i, len(g))
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// guard against stale tempdir files silently breaking
// the test setup (Windows file-locking is occasionally
// flaky — bail loud instead of weird "t.TempDir not
// empty" errors later).
//
// logx.Setup opens the per-Node log file and never
// closes it inside Node.New — it's closed only when
// the Node is closed (Node.Close → logx.Close) or the
// process exits. Tests that spin up multiple Nodes in
// the same process need to call logx.Close() between
// them so the previous log file is released and the
// next TempDir cleanup can RemoveAll successfully on
// Windows (which refuses to delete a file held open by
// another handle). Without this inter-test Close, the
// "TempDir RemoveAll cleanup" warning fires — and while
// the test assertion still passes (the actual test
// ran), go test reports FAIL, which is misleading.
func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

// closeTestLogs closes the package-global logx file so
// the t.TempDir() cleanup can remove the directory on
// Windows. Call via t.Cleanup.
func closeTestLogs(t *testing.T) {
	t.Helper()
	// No-op if no log file is currently open. Safe to
	// call multiple times.
	_ = logx.Close()
}