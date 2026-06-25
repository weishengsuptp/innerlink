package roster

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestStore_OpenMissingFile verifies that opening a
// nonexistent file returns an empty Store ready for
// use, not an error. This matches the alias / storage
// policy: no side effects until the user does
// something.
func TestStore_OpenMissingFile(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "roster.json")
	s, err := Open(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.List(); len(got) != 0 {
		t.Errorf("fresh store has %d entries, want 0", len(got))
	}
}

// TestStore_AddAndGet is the happy path: add an entry,
// retrieve it, verify fields survive a round-trip
// through Save + Open.
func TestStore_AddAndGet(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "roster.json")
	s, err := Open(tmp)
	if err != nil {
		t.Fatal(err)
	}
	added, err := s.Add(Entry{
		PeerID:   "0123456789abcdef0123456789abcdef",
		Hostname: "alice",
		Addrs:    []string{"192.168.40.5:4748"},
		Source:   "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !added {
		t.Error("Add on empty store: added=false, want true")
	}
	got, err := s.Get("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if got.Hostname != "alice" {
		t.Errorf("Hostname = %q, want alice", got.Hostname)
	}
	if len(got.Addrs) != 1 || got.Addrs[0] != "192.168.40.5:4748" {
		t.Errorf("Addrs = %v, want [192.168.40.5:4748]", got.Addrs)
	}
	if got.FirstSeen.IsZero() {
		t.Error("FirstSeen should be set by Add")
	}
	if got.LastSeen.IsZero() {
		t.Error("LastSeen should be set by Add")
	}
}

// TestStore_AddInvalidPeerID confirms the 32-char
// guard. We don't want a typo in the discovery layer
// to silently create a separate entry.
func TestStore_AddInvalidPeerID(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "roster.json")
	s, _ := Open(tmp)
	for _, bad := range []string{"", "abc", "0123456789abcdef0123456789abcde", "0123456789ABCDEF0123456789ABCDEF"} {
		_, err := s.Add(Entry{PeerID: bad, Hostname: "x"})
		if err == nil {
			t.Errorf("Add(%q) returned nil err, want validation failure", bad)
		}
	}
}

// TestStore_MergePreservesFirstSeen: when gossip brings
// us an entry we already have, the original first_seen
// must NOT be reset (otherwise the book "forgets" who
// was here first).
func TestStore_MergePreservesFirstSeen(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "roster.json")
	s, _ := Open(tmp)
	original := Entry{
		PeerID:   "0123456789abcdef0123456789abcdef",
		Hostname: "alice",
		Addrs:    []string{"192.168.40.5:4748"},
	}
	s.Add(original)
	first := s.List()[0].FirstSeen

	// Sleep a beat so a fresh FirstSeen would differ.
	// Then re-add via Add (not MergeFromGossip — same logic).
	s.Add(Entry{
		PeerID:   "0123456789abcdef0123456789abcdef",
		Hostname: "alice-laptop", // hostname changed
		Addrs:    []string{"192.168.40.5:4748", "10.0.0.5:4748"},
	})
	got := s.List()[0]
	if !got.FirstSeen.Equal(first) {
		t.Errorf("FirstSeen changed on re-Add: %v -> %v", first, got.FirstSeen)
	}
	if got.Hostname != "alice-laptop" {
		t.Errorf("Hostname = %q, want alice-laptop (refreshed)", got.Hostname)
	}
	if len(got.Addrs) != 2 {
		t.Errorf("Addrs len = %d, want 2 (refreshed)", len(got.Addrs))
	}
}

// TestStore_MergeFromGossip confirms the gossip path:
// new peers are added, existing peers are NOT
// refreshed (local direct observation is authoritative —
// gossip can be stale, and we have a direct channel
// that gives us fresher info), and malformed entries
// are silently skipped (defensive against a
// misbehaving peer).
func TestStore_MergeFromGossip(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "roster.json")
	s, _ := Open(tmp)
	// Pre-seed with peer X (we heard about it directly).
	s.Add(Entry{PeerID: "0123456789abcdef0123456789abcdef", Hostname: "x", Addrs: []string{"192.168.40.99:4748"}})

	// Gossip from B: introduces Y and Z, and tries to
	// "update" X with a different hostname. X should
	// be left alone (we have direct knowledge of X).
	remote := []Entry{
		{PeerID: "0123456789abcdef0123456789abcdef", Hostname: "x-refreshed", Addrs: []string{"192.168.40.1:4748"}},
		{PeerID: "fedcba9876543210fedcba9876543210", Hostname: "y", Addrs: []string{"192.168.40.2:4748"}},
		{PeerID: "11111111111111112222222222222222", Hostname: "z", Addrs: []string{"192.168.40.3:4748"}},
		{PeerID: "garbage", Hostname: "should-be-skipped"}, // malformed
	}
	res, err := s.MergeFromGossip(remote)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 2 {
		t.Errorf("newly added = %v, want 2 entries (y, z)", res.Added)
	}
	// X should NOT be refreshed by gossip — local direct
	// observation wins. This is the v0.5 design choice
	// (see MergeFromGossip doc comment).
	x, _ := s.Get("0123456789abcdef0123456789abcdef")
	if x.Hostname != "x" {
		t.Errorf("X.Hostname = %q, want x (gossip must not refresh existing entries)", x.Hostname)
	}
	// Malformed should NOT be present.
	if _, err := s.Get("garbage"); err == nil {
		t.Error("garbage entry was not skipped")
	}
}

// TestStore_Remove verifies Remove + ErrNotFound.
func TestStore_Remove(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "roster.json")
	s, _ := Open(tmp)
	s.Add(Entry{PeerID: "0123456789abcdef0123456789abcdef", Hostname: "x"})
	if err := s.Remove("0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("0123456789abcdef0123456789abcdef"); err != ErrNotFound {
		t.Errorf("after Remove, Get returned err = %v, want ErrNotFound", err)
	}
	// Removing again returns ErrNotFound.
	if err := s.Remove("0123456789abcdef0123456789abcdef"); err != ErrNotFound {
		t.Errorf("second Remove returned err = %v, want ErrNotFound", err)
	}
}

// TestStore_SaveAndReload is the round-trip test:
// Save, close, re-Open, verify state survives.
func TestStore_SaveAndReload(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "roster.json")
	s, _ := Open(tmp)
	s.Add(Entry{PeerID: "0123456789abcdef0123456789abcdef", Hostname: "alice", Addrs: []string{"1.2.3.4:4748"}})
	s.Add(Entry{PeerID: "fedcba9876543210fedcba9876543210", Hostname: "bob", Addrs: []string{"1.2.3.5:4748"}})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.List(); len(got) != 2 {
		t.Errorf("reloaded store has %d entries, want 2", len(got))
	}
}

// TestStore_ConcurrentAccess is the race detector
// smoke test. The Store claims to be safe for
// concurrent use; this test fails the race detector
// if that claim is false. Run with -race.
func TestStore_ConcurrentAccess(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "roster.json")
	s, _ := Open(tmp)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(3)
		go func(i int) {
			defer wg.Done()
			pid := strings.Repeat("a", 30) + string(rune('a'+i)) + string(rune('0'))
			s.Add(Entry{PeerID: pid, Hostname: "x"})
		}(i)
		go func() {
			defer wg.Done()
			s.List()
		}()
		go func() {
			defer wg.Done()
			_ = s.Save()
		}()
	}
	wg.Wait()
}

// TestStore_ListSorted: List must return entries
// sorted by PeerID, for stable on-disk and gossip
// payloads. Easier to diff in tests and logs.
func TestStore_ListSorted(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "roster.json")
	s, _ := Open(tmp)
	// Add in random order.
	s.Add(Entry{PeerID: "ffffffffffffffffffffffffffffffff", Hostname: "f"})
	s.Add(Entry{PeerID: "00000000000000000000000000000000", Hostname: "0"})
	s.Add(Entry{PeerID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Hostname: "a"})

	got := s.List()
	if len(got) != 3 {
		t.Fatalf("List returned %d entries, want 3", len(got))
	}
	want := []string{
		"00000000000000000000000000000000",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"ffffffffffffffffffffffffffffffff",
	}
	for i, w := range want {
		if got[i].PeerID != w {
			t.Errorf("List[%d].PeerID = %q, want %q", i, got[i].PeerID, w)
		}
	}
}

// TestStore_DedupResetOnMerge (2026-06-24+): when a
// new (peerID, IP, hostname) arrives via gossip that
// collides on (IP, hostname) with an existing OFFLINE
// entry having a DIFFERENT peerID, the existing entry
// gets Reset=true. The new entry is added normally.
// The reset marker is sticky — even if the new entry
// comes back with a stale state, the old one stays
// reset.
//
// Scenario: VM was wiped + reinstalled (same IP,
// same hostname, new peerID). Old peerID's ghost must
// not flicker back into the UI when the VM comes back
// online.
func TestStore_DedupResetOnMerge(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "roster.json")
	s, _ := Open(tmp)

	// Old ghost: peer A used to be at this IP+hostname,
	// now offline. We'll seed it directly via Add.
	const ghostID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const newID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	s.Add(Entry{
		PeerID:   ghostID,
		Hostname: "vm-1",
		Addrs:    []string{"192.168.40.5:4748"},
	})

	// Gossip arrives: new peerID B, same hostname, same IP.
	remote := []Entry{{
		PeerID:   newID,
		Hostname: "vm-1",
		Addrs:    []string{"192.168.40.5:4748"},
	}}
	res, err := s.MergeFromGossip(remote)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 1 || res.Added[0] != newID {
		t.Errorf("newly added = %v, want [%s]", res.Added, newID)
	}

	// Old ghost should be reset.
	ghost, _ := s.Get(ghostID)
	if !ghost.Reset {
		t.Errorf("old peerID %s not marked reset after dedup merge", ghostID)
	}
	// New entry should be present and not reset.
	fresh, _ := s.Get(newID)
	if fresh.Reset {
		t.Errorf("new peerID %s should not be reset", newID)
	}
	if fresh.Hostname != "vm-1" {
		t.Errorf("new Hostname = %q, want vm-1", fresh.Hostname)
	}

	// ListActive() filters the ghost out; List() keeps it
	// (on-disk source of truth).
	active := s.ListActive()
	found := false
	for _, e := range active {
		if e.PeerID == ghostID {
			found = true
		}
	}
	if found {
		t.Errorf("ListActive still includes reset ghost %s", ghostID)
	}
	all := s.List()
	found = false
	for _, e := range all {
		if e.PeerID == ghostID {
			found = true
		}
	}
	if !found {
		t.Errorf("List does not include ghost %s (on-disk source of truth)", ghostID)
	}
}

// TestStore_DedupOnlyWhenColliding: dedup is keyed on
// (IP, hostname) BOTH matching. Same hostname but
// different IP must NOT dedup (could be a separate
// machine that happens to share a name). Same IP but
// different hostname must NOT dedup either.
func TestStore_DedupOnlyWhenColliding(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "roster.json")
	s, _ := Open(tmp)
	const oldA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const oldB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	// Ghost: same hostname, different IP.
	s.Add(Entry{PeerID: oldA, Hostname: "shared-host", Addrs: []string{"10.0.0.1:4748"}})
	// Ghost: different hostname, same IP.
	s.Add(Entry{PeerID: oldB, Hostname: "different-host", Addrs: []string{"192.168.40.5:4748"}})

	// New entry: shared-host + 192.168.40.5 — neither
	// ghost matches both fields.
	s.MergeFromGossip([]Entry{{
		PeerID:   "cccccccccccccccccccccccccccccccc",
		Hostname: "shared-host",
		Addrs:    []string{"192.168.40.5:4748"},
	}})

	for _, id := range []string{oldA, oldB} {
		e, _ := s.Get(id)
		if e.Reset {
			t.Errorf("ghost %s wrongly marked reset (collided on only one field)", id)
		}
	}
}

// TestStore_DedupIsOneShot: a second collision on the
// same ghost does NOT re-trigger any work, AND any
// subsequent state change on the ghost (e.g. another
// Add with a different hostname) does NOT un-reset it.
// "一次性, 防止状态反复" — user requirement.
func TestStore_DedupIsOneShot(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "roster.json")
	s, _ := Open(tmp)
	const ghostID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const newID1 = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const newID2 = "cccccccccccccccccccccccccccccccc"

	s.Add(Entry{PeerID: ghostID, Hostname: "vm-1", Addrs: []string{"192.168.40.5:4748"}})
	// First collision: ghost gets reset.
	s.MergeFromGossip([]Entry{{PeerID: newID1, Hostname: "vm-1", Addrs: []string{"192.168.40.5:4748"}}})
	if e, _ := s.Get(ghostID); !e.Reset {
		t.Fatal("ghost not reset after first collision")
	}

	// Second collision: nothing changes.
	s.MergeFromGossip([]Entry{{PeerID: newID2, Hostname: "vm-1", Addrs: []string{"192.168.40.5:4748"}}})
	ghost, _ := s.Get(ghostID)
	if !ghost.Reset {
		t.Errorf("ghost reset state lost after second collision")
	}

	// Re-Add the ghost (e.g. some other code path
	// touches it). Reset must stick.
	s.Add(Entry{PeerID: ghostID, Hostname: "vm-1-updated", Addrs: []string{"192.168.40.5:4748"}})
	ghost, _ = s.Get(ghostID)
	if !ghost.Reset {
		t.Errorf("ghost Reset cleared by Add — must be sticky")
	}
}

// TestStore_MarkReset exercises the explicit API as
// well as the idempotency contract: marking twice is
// a no-op, marking a missing peerID is an error.
func TestStore_MarkReset(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "roster.json")
	s, _ := Open(tmp)
	const pid = "0123456789abcdef0123456789abcdef"
	s.Add(Entry{PeerID: pid, Hostname: "x"})

	// First mark: success.
	if err := s.MarkReset(pid); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.Get(pid); !e.Reset {
		t.Errorf("after MarkReset, entry.Reset = false")
	}
	// Second mark: idempotent no-op (no error).
	if err := s.MarkReset(pid); err != nil {
		t.Errorf("second MarkReset returned err = %v, want nil", err)
	}
	// Missing peerID: ErrNotFound.
	if err := s.MarkReset("ffffffffffffffffffffffffffffffff"); err != ErrNotFound {
		t.Errorf("MarkReset on missing peerID returned err = %v, want ErrNotFound", err)
	}
}

// TestStore_OpenV1Compat: a v1 file (no alias, no
// reset) must still load cleanly. The schema bumped
// to v2 in 2026-06-24 to add alias + reset; v1 files
// from before that must still work.
func TestStore_OpenV1Compat(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "roster.json")
	// Hand-craft a v1 file with one entry.
	const v1 = `{
  "v": 1,
  "entries": {
    "0123456789abcdef0123456789abcdef": {
      "peer_id": "0123456789abcdef0123456789abcdef",
      "hostname": "legacy-host",
      "addrs": ["192.168.40.5:4748"],
      "first_seen": "2025-12-31T23:59:59Z",
      "last_seen": "2025-12-31T23:59:59Z"
    }
  }
}`
	if err := os.WriteFile(tmp, []byte(v1), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(tmp)
	if err != nil {
		t.Fatalf("Open on v1 file: %v", err)
	}
	e, err := s.Get("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("Get on legacy entry: %v", err)
	}
	if e.Hostname != "legacy-host" {
		t.Errorf("legacy Hostname = %q, want legacy-host", e.Hostname)
	}
	if e.Alias != "" {
		t.Errorf("legacy Alias = %q, want empty", e.Alias)
	}
	if e.Reset {
		t.Errorf("legacy Reset = true, want false")
	}
}
