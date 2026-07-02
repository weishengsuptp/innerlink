package roster

// audit_test.go — pins down AuditStaleTombstones behaviour.
//
// v1.1.4 (2026-07-02) hotfix: pre-fix, reset=true entries
// stayed on disk forever. AuditStaleTombstones drops the ones
// older than maxAge so the file size stops growing across
// wipe+reinstall cycles. The tests below cover the "drop"
// branch, the "keep recent" branch, the "non-reset" branch,
// and the "zero LastSeen" defensive branch.

import (
	"path/filepath"
	"testing"
	"time"
)

func newStoreWithTombstones(t *testing.T, maxAge time.Duration) *Store {
	t.Helper()
	tmp := t.TempDir()
	s, err := Open(filepath.Join(tmp, "roster.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// Active entry (non-reset, recent) — must NOT be dropped.
	if _, err := s.Add(Entry{
		PeerID:    "aaaa1111aaaa1111aaaa1111aaaa1111",
		Hostname:  "alive",
		Addrs:     []string{"192.168.1.1:4748"},
		FirstSeen: now.Add(-1 * time.Hour),
		LastSeen:  now.Add(-1 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	// Reset entry, OLD (5x maxAge ago) — MUST be dropped.
	if _, err := s.Add(Entry{
		PeerID:    "bbbb2222bbbb2222bbbb2222bbbb2222",
		Hostname:  "ghost-old",
		Addrs:     []string{"192.168.1.2:4748"},
		FirstSeen: now.Add(-10 * maxAge),
		LastSeen:  now.Add(-5 * maxAge),
		Reset:     true,
	}); err != nil {
		t.Fatal(err)
	}
	// Reset entry, FRESH (recent) — must NOT be dropped.
	if _, err := s.Add(Entry{
		PeerID:    "cccc3333cccc3333cccc3333cccc3333",
		Hostname:  "ghost-fresh",
		Addrs:     []string{"192.168.1.3:4748"},
		FirstSeen: now,
		LastSeen:  now,
		Reset:     true,
	}); err != nil {
		t.Fatal(err)
	}
	// Reset entry, ZERO LastSeen — must NOT be dropped
	// (defensive: no age signal means we can't tell if
	// it's old).
	if _, err := s.Add(Entry{
		PeerID:   "dddd4444dddd4444dddd4444dddd4444",
		Hostname: "ghost-zero",
		Addrs:    []string{"192.168.1.4:4748"},
		Reset:    true,
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAuditStaleTombstones_DropsOldResets(t *testing.T) {
	s := newStoreWithTombstones(t, 24*time.Hour)
	dropped := s.AuditStaleTombstones(24 * time.Hour)
	if len(dropped) != 1 {
		t.Fatalf("dropped len = %d, want 1; got %v", len(dropped), dropped)
	}
	if dropped[0] != "bbbb2222bbbb2222bbbb2222bbbb2222" {
		t.Errorf("dropped[0] = %q, want old reset peer", dropped[0])
	}
	// Confirm in-memory map reflects the drop.
	if _, err := s.Get("bbbb2222bbbb2222bbbb2222bbbb2222"); err == nil {
		t.Error("old reset entry still in map after audit, want gone")
	}
	// And the other three survive.
	for _, pid := range []string{
		"aaaa1111aaaa1111aaaa1111aaaa1111",
		"cccc3333cccc3333cccc3333cccc3333",
		"dddd4444dddd4444dddd4444dddd4444",
	} {
		if _, err := s.Get(pid); err != nil {
			t.Errorf("entry %q should have survived audit, got err=%v", pid, err)
		}
	}
}

func TestAuditStaleTombstones_SetsDirty(t *testing.T) {
	tmp := t.TempDir()
	s, err := Open(filepath.Join(tmp, "r.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := s.Add(Entry{
		PeerID:   "eeee5555eeee5555eeee5555eeee5555",
		Hostname: "h",
		Addrs:    []string{"1.2.3.4:4748"},
		LastSeen: now.Add(-100 * time.Hour),
		Reset:    true,
	}); err != nil {
		t.Fatal(err)
	}
	// Add sets dirty. Run audit; it should ALSO mark dirty
	// (a drop happened). Save the file and confirm the
	// on-disk state has the drop.
	dropped := s.AuditStaleTombstones(24 * time.Hour)
	if len(dropped) != 1 {
		t.Fatalf("dropped len = %d, want 1", len(dropped))
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	// Reload from disk; the old entry must be gone.
	s2, err := Open(filepath.Join(tmp, "r.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Get("eeee5555eeee5555eeee5555eeee5555"); err == nil {
		t.Error("old reset entry survived Save, want dropped on disk")
	}
}

func TestAuditStaleTombstones_NoOpOnEmpty(t *testing.T) {
	tmp := t.TempDir()
	s, _ := Open(filepath.Join(tmp, "r.json"))
	if dropped := s.AuditStaleTombstones(24 * time.Hour); len(dropped) != 0 {
		t.Errorf("dropped on empty = %v, want nil", dropped)
	}
}
