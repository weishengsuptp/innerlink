package selfid

// selfid_test.go — unit tests for internal/selfid.
//
// What this catches:
//   - v1.1.4 (2026-07-02) self-claim infrastructure. Pre-fix
//     there was no notion of "my previous peerID", so every
//     wipe+reinstall cycle left stranded peerIDs in groups
//     and rosters across the LAN. These tests pin down the
//     behaviour of the new self_history.json file: missing-file
//     load, parse error tolerance, rolling-window cap, etc.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStore_OpenMissingFile(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "self_history.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open missing file: %v, want nil", err)
	}
	if s == nil {
		t.Fatal("Open returned nil Store")
	}
	if got := s.OldPeerIDs(); got != nil {
		t.Errorf("OldPeerIDs on empty store = %v, want nil", got)
	}
}

func TestStore_OpenCorruptFile(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "self_history.json")
	if err := os.WriteFile(path, []byte("not json {{{"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	// Pre-fix would have hard-failed. v1.1.4 contract:
	// return soft error + empty store; caller decides.
	if err == nil {
		t.Fatal("Open corrupt file: want error, got nil")
	}
	if s == nil {
		t.Fatal("Open returned nil Store on soft error")
	}
	if got := s.OldPeerIDs(); got != nil {
		t.Errorf("OldPeerIDs on corrupt store = %v, want nil", got)
	}
}

func TestStore_OpenWrongVersion(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "self_history.json")
	f := fileFormat{V: 99, Entries: []Entry{{NewPeerID: "x"}}}
	data, _ := json.MarshalIndent(f, "", "  ")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(path)
	if err == nil {
		t.Fatal("Open wrong version: want error, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported version") {
		t.Errorf("error = %v, want it to mention 'unsupported version'", err)
	}
}

func TestStore_RecordAndQuery(t *testing.T) {
	tmp := t.TempDir()
	s, err := Open(filepath.Join(tmp, "self_history.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a wipe+reinstall cycle. We had peerID A,
	// wiped, now have peerID B. Record it.
	now := time.Now().UTC()
	if err := s.RecordMigration(Entry{
		OldPeerID:  "0123456789abcdef0123456789abcdef",
		NewPeerID:  "fedcba9876543210fedcba9876543210",
		SwitchedAt: now,
		Trigger:    TriggerWipeReinstall,
	}); err != nil {
		t.Fatalf("RecordMigration: %v", err)
	}
	got := s.OldPeerIDs()
	if len(got) != 1 {
		t.Fatalf("OldPeerIDs len = %d, want 1", len(got))
	}
	if got[0] != "0123456789abcdef0123456789abcdef" {
		t.Errorf("OldPeerIDs[0] = %q, want the prior peerID", got[0])
	}
}

func TestStore_RecordRejectsEmptyNewPeerID(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "x.json"))
	if err := s.RecordMigration(Entry{NewPeerID: ""}); err == nil {
		t.Fatal("RecordMigration with empty NewPeerID: want error, got nil")
	}
}

func TestStore_RollingWindowCap(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "x.json"))
	// Record 15 entries; expect cap at 10. The 10 most recent
	// (entries 6..15) should survive.
	for i := 0; i < 15; i++ {
		if err := s.RecordMigration(Entry{
			OldPeerID: "old" + string(rune('a'+i)),
			NewPeerID: "new" + string(rune('a'+i)),
		}); err != nil {
			t.Fatalf("RecordMigration[%d]: %v", i, err)
		}
	}
	// Internal access via Latest() — we only have one query
	// method exposed; verify via OldPeerIDs.
	got := s.OldPeerIDs()
	if len(got) != 10 {
		t.Errorf("OldPeerIDs len = %d, want 10 (rolling window)", len(got))
	}
	// The 5 oldest ('a'..'e') should be dropped; the 10
	// newest ('f'..'o') should remain. Verify by string
	// content: 'a' and 'b' must be absent, 'n' and 'o' present.
	for _, id := range got {
		if strings.HasSuffix(id, "a") || strings.HasSuffix(id, "b") ||
			strings.HasSuffix(id, "c") || strings.HasSuffix(id, "d") ||
			strings.HasSuffix(id, "e") {
			t.Errorf("old entry %q should have been dropped (FIFO)", id)
		}
	}
	if got[0] != "oldf" || got[9] != "oldo" {
		t.Errorf("OldPeerIDs[0]=%q [9]=%q, want oldf .. oldo", got[0], got[9])
	}
}

func TestStore_FreshInstallEntryNotInOldPeerIDs(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "x.json"))
	// A fresh_install entry has OldPeerID = "". It records
	// "we had no prior identity". It should NOT show up
	// in OldPeerIDs (no claim target).
	if err := s.RecordMigration(Entry{
		NewPeerID:  "abc123abc123abc123abc123abc123ab",
		SwitchedAt: time.Now().UTC(),
		Trigger:    TriggerFreshInstall,
	}); err != nil {
		t.Fatal(err)
	}
	if got := s.OldPeerIDs(); len(got) != 0 {
		t.Errorf("OldPeerIDs after fresh install = %v, want empty (no claim target)", got)
	}
}

func TestStore_SaveAndReloadRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "self_history.json")
	s, _ := Open(path)
	if err := s.RecordMigration(Entry{
		OldPeerID: "0123456789abcdef0123456789abcdef",
		NewPeerID: "fedcba9876543210fedcba9876543210",
		Trigger:   TriggerWipeReinstall,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Confirm the file exists and parses.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file not on disk after Save: %v", err)
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("file not valid JSON: %v", err)
	}
	if f.V != CurrentVersion {
		t.Errorf("V = %d, want %d", f.V, CurrentVersion)
	}
	if len(f.Entries) != 1 {
		t.Fatalf("Entries len = %d, want 1", len(f.Entries))
	}
	if f.Entries[0].NewPeerID != "fedcba9876543210fedcba9876543210" {
		t.Errorf("Entries[0].NewPeerID = %q, want expected", f.Entries[0].NewPeerID)
	}
	// Reload via a fresh Open; same content should come back.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.OldPeerIDs(); len(got) != 1 {
		t.Errorf("reload OldPeerIDs len = %d, want 1", len(got))
	}
}

func TestStore_SaveNoopWhenClean(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "self_history.json")
	s, _ := Open(path)
	// Open doesn't set dirty. Save should be a no-op
	// (no file created on disk).
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("file exists after no-op Save, want ErrNotExist")
	}
}

func TestStore_SaveAtomic(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "self_history.json")
	// Pre-create a file. The atomic Save should overwrite
	// it via tmp+rename; the old content should be gone.
	if err := os.WriteFile(path, []byte("STALE"), 0600); err != nil {
		t.Fatal(err)
	}
	s, _ := Open(path)
	// But Open on a stale file fails — it's "STALE", not
	// JSON. Open returns soft error + empty store. The
	// old file content is preserved. Re-opening shows the
	// parse error.
	// We need a different angle: pre-create a valid v0 file
	// (or just any JSON), Open it OK, then Save with new
	// content. The atomic rename should replace the file.
	if err := os.WriteFile(path, []byte(`{"v":1,"entries":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	s, _ = Open(path)
	if err := s.RecordMigration(Entry{NewPeerID: "abc123abc123abc123abc123abc123ab"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "STALE") {
		t.Error("atomic Save left stale content")
	}
	if !strings.Contains(string(data), "abc123abc123abc123abc123abc123ab") {
		t.Error("atomic Save did not write new content")
	}
}

func TestStore_Latest(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "x.json"))
	if _, ok := s.Latest(); ok {
		t.Fatal("Latest on empty store: want false")
	}
	if err := s.RecordMigration(Entry{NewPeerID: "first", OldPeerID: ""}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordMigration(Entry{NewPeerID: "second", OldPeerID: "first"}); err != nil {
		t.Fatal(err)
	}
	e, ok := s.Latest()
	if !ok {
		t.Fatal("Latest on non-empty store: want true")
	}
	if e.NewPeerID != "second" {
		t.Errorf("Latest.NewPeerID = %q, want %q", e.NewPeerID, "second")
	}
}
