package group

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// makeMembers builds a 2-member roster for tests.
func makeMembers(t *testing.T) (*Members, []byte) {
	t.Helper()
	creator := []byte("creator-peer-id-16b")
	now := time.Now().UTC()
	gid := ComputeGroupID(creator, "test group", now)
	m := &Members{
		GroupID:   RenderGroupID(gid),
		GroupName: "test group",
		Creator:   "creator-peer-id",
		CreatedAt: now,
		Members: []Member{
			{PeerID: "creator-peer-id", Alias: "Alice", JoinedAt: now, IsCreator: true},
			{PeerID: "invitee-peer-id", Alias: "Bob", JoinedAt: now.Add(time.Second)},
		},
	}
	return m, gid
}

func TestMembersSaveLoadRoundtrip(t *testing.T) {
	dir := t.TempDir()
	m, gid := makeMembers(t)
	if err := m.Save(dir, gid); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// File should exist at the expected path. Permission checks
	// are skipped on Windows because os.WriteFile's permission
	// bits aren't enforced the same way — the test asserts the
	// path + roundtrip, which is the actual correctness contract.
	path := filepath.Join(dir, "groups", RenderGroupID(gid), "members.json")
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat members.json: %v", err)
	}
	if st.Size() == 0 {
		t.Errorf("members.json is empty")
	}
	if runtime.GOOS != "windows" {
		// POSIX systems honor the 0600 we requested.
		if got := st.Mode().Perm(); got != 0o600 {
			t.Errorf("members.json perm = %o, want 0600", got)
		}
	}
	loaded, err := LoadMembers(dir, gid)
	if err != nil {
		t.Fatalf("LoadMembers: %v", err)
	}
	if loaded.GroupID != m.GroupID || loaded.GroupName != m.GroupName {
		t.Errorf("roundtrip: %+v vs %+v", loaded, m)
	}
	if len(loaded.Members) != len(m.Members) {
		t.Fatalf("Members len: got %d, want %d", len(loaded.Members), len(m.Members))
	}
}

func TestMembersSaveRejectsBadGroupIDSize(t *testing.T) {
	dir := t.TempDir()
	m := &Members{GroupID: "x"}
	for _, bad := range [][]byte{nil, make([]byte, 16), make([]byte, 64)} {
		if err := m.Save(dir, bad); err == nil {
			t.Errorf("Save accepted bad GroupID size %d", len(bad))
		}
	}
}

func TestMembersLoadRejectsBadGroupIDSize(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range [][]byte{nil, make([]byte, 16)} {
		if _, err := LoadMembers(dir, bad); err == nil {
			t.Errorf("LoadMembers accepted bad GroupID size %d", len(bad))
		}
	}
}

func TestMembersContains(t *testing.T) {
	m, _ := makeMembers(t)
	if !m.Contains("creator-peer-id") {
		t.Error("Contains(creator) = false")
	}
	if !m.Contains("invitee-peer-id") {
		t.Error("Contains(invitee) = false")
	}
	if m.Contains("ghost-peer") {
		t.Error("Contains(ghost) = true")
	}
}

func TestMembersAddMember(t *testing.T) {
	m, _ := makeMembers(t)
	now := time.Now().UTC()
	if err := m.AddMember(Member{PeerID: "new-peer", Alias: "Carol", JoinedAt: now}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if !m.Contains("new-peer") {
		t.Error("after add, Contains(new-peer) = false")
	}
	// Adding the same peer again is an error.
	if err := m.AddMember(Member{PeerID: "new-peer", JoinedAt: now}); err == nil {
		t.Error("AddMember duplicate accepted")
	}
	// Empty peerID is an error.
	if err := m.AddMember(Member{PeerID: ""}); err == nil {
		t.Error("AddMember empty peerID accepted")
	}
}

func TestMembersAddMemberSortsByJoinedAt(t *testing.T) {
	m, _ := makeMembers(t)
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	// Add out of order; expect them sorted by JoinedAt.
	if err := m.AddMember(Member{PeerID: "late", JoinedAt: base.Add(72 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := m.AddMember(Member{PeerID: "mid", JoinedAt: base.Add(48 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := m.AddMember(Member{PeerID: "early", JoinedAt: base.Add(24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// Walk Members and confirm JoinedAt is non-decreasing.
	// We can't do a stable name-based sort check (creator is
	// inserted at the start with its own JoinedAt) — but we
	// can verify that "early" comes before "mid" comes before
	// "late" in the slice.
	pos := map[string]int{}
	for i, mem := range m.Members {
		pos[mem.PeerID] = i
	}
	if pos["early"] >= pos["mid"] {
		t.Errorf("early (pos %d) should come before mid (pos %d)", pos["early"], pos["mid"])
	}
	if pos["mid"] >= pos["late"] {
		t.Errorf("mid (pos %d) should come before late (pos %d)", pos["mid"], pos["late"])
	}
}

func TestMembersRemoveMember(t *testing.T) {
	m, _ := makeMembers(t)
	// Creator cannot be removed.
	if m.RemoveMember("creator-peer-id") {
		t.Error("RemoveMember(creator) returned true; creators should be protected")
	}
	if !m.Contains("creator-peer-id") {
		t.Error("creator removed despite protection")
	}
	// Non-creator can be removed.
	if !m.RemoveMember("invitee-peer-id") {
		t.Error("RemoveMember(invitee) returned false")
	}
	if m.Contains("invitee-peer-id") {
		t.Error("invitee still present after remove")
	}
	// Removing a non-existent peer returns false.
	if m.RemoveMember("ghost") {
		t.Error("RemoveMember(ghost) returned true")
	}
}
