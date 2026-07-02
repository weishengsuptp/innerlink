package leavelog

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestOpen_MissingFileReturnsEmpty is the cold-start path:
// the device has never left a group, so the file doesn't
// exist. Open must return an empty Store (no error) so
// Node.New can proceed without a "log file missing" panic.
func TestOpen_MissingFileReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leaved_groups.json")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open missing file: got error %v, want nil", err)
	}
	if got := len(s.List()); got != 0 {
		t.Errorf("Open missing file: List() len = %d, want 0", got)
	}
}

// TestRecordAndSave is the happy path: Record 1 entry,
// Save, re-Open, verify the entry is on disk.
func TestRecordAndSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leaved_groups.json")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Record(Entry{GroupID: "g_abc", LeftAt: time.Now().UTC()}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Verify on-disk JSON shape so a future schema bump
	// catches a regression here rather than at e2e.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !contains(data, `"v": 1`) {
		t.Errorf("on-disk JSON missing schema version 1, got: %s", data)
	}
	if !contains(data, `"g_abc"`) {
		t.Errorf("on-disk JSON missing recorded group, got: %s", data)
	}

	// Re-Open, verify List returns the entry.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	all := s2.List()
	if len(all) != 1 {
		t.Fatalf("re-Open List len = %d, want 1", len(all))
	}
	if all[0].GroupID != "g_abc" {
		t.Errorf("re-Open List[0].GroupID = %q, want g_abc", all[0].GroupID)
	}
}

// TestRecord_EmptyGroupIDRejected pins down the
// caller-bug check. An empty GroupID is meaningless and
// would produce a row we can't dedup or replay, so we
// refuse to record it.
func TestRecord_EmptyGroupIDRejected(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(filepath.Join(dir, "leaved_groups.json"))
	if err := s.Record(Entry{GroupID: ""}); err == nil {
		t.Fatal("Record(empty GroupID) returned nil, want error")
	}
}

// TestSave_DedupesSameGroupID pins down the "leave, rejoin,
// leave" cycle: the most recent row for a GroupID is what
// survives, not all the historical events. A creator
// receiving the replay only needs to know "currently
// outside this group", not "this is the 3rd time A has
// left".
func TestSave_DedupesSameGroupID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leaved_groups.json")

	s, _ := Open(path)
	// Three leaves of the same group, in chronological
	// order. Save should keep only the most recent.
	s.Record(Entry{GroupID: "g_abc", LeftAt: time.Unix(1, 0).UTC()})
	s.Record(Entry{GroupID: "g_abc", LeftAt: time.Unix(2, 0).UTC()})
	s.Record(Entry{GroupID: "g_abc", LeftAt: time.Unix(3, 0).UTC()})
	// Plus an unrelated group, which should survive.
	s.Record(Entry{GroupID: "g_xyz", LeftAt: time.Unix(2, 0).UTC()})

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	all := s.List()
	if len(all) != 2 {
		t.Fatalf("List len = %d, want 2 (deduped g_abc + g_xyz); got %+v", len(all), all)
	}
	// g_abc must be the LATER of the two (LeftAt == 3).
	// The implementation also reorders so the file reads
	// chronologically; we don't pin that here, only that
	// the surviving g_abc is the most-recent one.
	var foundABC bool
	for _, e := range all {
		if e.GroupID == "g_abc" {
			foundABC = true
			if e.LeftAt.Unix() != 3 {
				t.Errorf("g_abc LeftAt = %d, want 3 (most recent)", e.LeftAt.Unix())
			}
		}
	}
	if !foundABC {
		t.Error("g_abc disappeared after dedup, want it preserved")
	}
}

// TestSave_NoopWhenNotDirty pins down the "Save called
// twice, no Record between" path. The second Save must
// not touch disk (no rewrite of identical content, no
// atime bump). We don't have a clean way to assert
// "no write happened" without spying on os.WriteFile, so
// we just assert it doesn't error and that the in-memory
// state is unchanged.
func TestSave_NoopWhenNotDirty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leaved_groups.json")
	s, _ := Open(path)
	s.Record(Entry{GroupID: "g_abc", LeftAt: time.Unix(1, 0).UTC()})
	if err := s.Save(); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	// Capture mtime to detect a redundant rewrite.
	t0 := fileMod(t, path)
	time.Sleep(20 * time.Millisecond)
	if err := s.Save(); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	t1 := fileMod(t, path)
	if !t0.Equal(t1) {
		t.Errorf("second Save rewrote file (mtime %v -> %v); expected no-op", t0, t1)
	}
}

// TestSave_TrimsToCap pins down the rolling-window
// cap. We Record >maxEntries, then Save, and assert the
// oldest ones got dropped.
func TestSave_TrimsToCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leaved_groups.json")
	s, _ := Open(path)
	// maxEntries + 10, oldest 10 should be trimmed.
	for i := 0; i < maxEntries+10; i++ {
		s.Record(Entry{
			GroupID: groupIDFor(i),
			LeftAt:  time.Unix(int64(i), 0).UTC(),
		})
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	all := s.List()
	if len(all) != maxEntries {
		t.Errorf("List len = %d, want %d (cap)", len(all), maxEntries)
	}
	// The OLDEST surviving should be the entry with
	// i = 10 (we trimmed i=0..9).
	if all[0].GroupID != groupIDFor(10) {
		t.Errorf("after trim, oldest = %q, want %q", all[0].GroupID, groupIDFor(10))
	}
}

// TestContains pins down the lookup helper used by
// ApplyRosterUpdate to short-circuit "don't re-create
// this group on my disk, I already left it".
func TestContains(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(filepath.Join(dir, "leaved_groups.json"))
	s.Record(Entry{GroupID: "g_abc", LeftAt: time.Now().UTC()})
	if !s.Contains("g_abc") {
		t.Error("Contains(g_abc) = false after Record, want true")
	}
	if s.Contains("g_xyz") {
		t.Error("Contains(g_xyz) = true for unrecorded group, want false")
	}
}

// TestConcurrentRecord is a small smoke test for the
// locking: Record from many goroutines, Save once, all
// entries should land on disk.
func TestConcurrentRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leaved_groups.json")
	s, _ := Open(path)

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			s.Record(Entry{
				GroupID: groupIDFor(i),
				LeftAt:  time.Unix(int64(i), 0).UTC(),
			})
		}(i)
	}
	wg.Wait()
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	all := s.List()
	if len(all) != N {
		t.Errorf("List len = %d, want %d", len(all), N)
	}
}

// TestParseFailureReturnsEmpty pins down the soft-error
// path: a corrupt file should not panic, should return
// (empty store, error) so Node.New can log + continue.
// We assert the caller-facing contract, not the exact log.
func TestParseFailureReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "leaved_groups.json")
	if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	s, err := Open(path)
	if err == nil {
		t.Fatal("Open corrupt file: got nil error, want non-nil")
	}
	if got := len(s.List()); got != 0 {
		t.Errorf("Open corrupt: List len = %d, want 0", got)
	}
}

// --- helpers ---

func contains(haystack []byte, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == needle {
			return true
		}
	}
	return false
}

func fileMod(t *testing.T, path string) time.Time {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	return st.ModTime()
}

func groupIDFor(i int) string {
	// Cheap unique GroupID per index. We don't use the
	// real "g_<64hex>" format here because the leavelog
	// only treats it as an opaque string.
	return "g_test_" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
