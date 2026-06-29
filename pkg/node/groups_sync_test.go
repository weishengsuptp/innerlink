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
	// Schedule a logx.Close on test cleanup so the temp
	// dir RemoveAll on Windows can actually delete the
	// logfile (logx.Setup holds an exclusive write handle).
	t.Cleanup(func() { _ = logx.Close() })
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
	// Schedule a final logx.Close — needed because the
	// second node opened its own logfile which would
	// otherwise stay held when t.TempDir cleans up.
	t.Cleanup(func() { _ = logx.Close() })
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