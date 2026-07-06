package witnesslog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestOpen_MissingFileReturnsEmpty is the cold-start path:
// the device has never witnessed a leave, so the file
// doesn't exist. Open must return an empty Store (no
// error) so Node.New can proceed without a "log file
// missing" panic.
func TestOpen_MissingFileReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "witnessed_leaves.json")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open missing file: got error %v, want nil", err)
	}
	if got := len(s.List()); got != 0 {
		t.Errorf("Open missing file: List() len = %d, want 0", got)
	}
}

// TestRecordAndSave is the happy path: Record 1 entry,
// Save, re-Open, verify the entry is on disk and the
// (GroupID, LeaverID) tuple round-trips.
func TestRecordAndSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "witnessed_leaves.json")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Record(Entry{
		GroupID:     "g_abc",
		LeaverID:    "peerA",
		WitnessedAt: time.Now().UTC(),
	}); err != nil {
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
	if !contains(data, `"peerA"`) {
		t.Errorf("on-disk JSON missing recorded leaver, got: %s", data)
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
	if all[0].GroupID != "g_abc" || all[0].LeaverID != "peerA" {
		t.Errorf("re-Open List[0] = %+v, want {g_abc, peerA}", all[0])
	}
	// On-disk JSON parses as the fileFormat struct.
	var ff fileFormat
	if err := json.Unmarshal(data, &ff); err != nil {
		t.Errorf("on-disk JSON does not parse as fileFormat: %v", err)
	}
	if ff.V != CurrentVersion {
		t.Errorf("on-disk schema version = %d, want %d", ff.V, CurrentVersion)
	}
}

// TestRecord_EmptyGroupIDRejected pins down the
// caller-bug check. An empty GroupID is meaningless and
// would produce a row we can't dedup or replay, so we
// refuse to record it.
func TestRecord_EmptyGroupIDRejected(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(filepath.Join(dir, "witnessed_leaves.json"))
	if err := s.Record(Entry{GroupID: "", LeaverID: "peerA"}); err == nil {
		t.Fatal("Record(empty GroupID) returned nil, want error")
	}
}

// TestRecord_EmptyLeaverIDRejected — unlike leavelog
// (where LeaverID is implicit = self), witnesslog MUST
// have an explicit LeaverID or the relay doesn't know
// whose leave to push.
func TestRecord_EmptyLeaverIDRejected(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(filepath.Join(dir, "witnessed_leaves.json"))
	if err := s.Record(Entry{GroupID: "g_abc", LeaverID: ""}); err == nil {
		t.Fatal("Record(empty LeaverID) returned nil, want error")
	}
}

// TestSave_DedupesSameTuple pins down the "I see the same
// leave relayed 5 times" case. Each (GroupID, LeaverID)
// pair keeps only the most recent row on disk. This is
// the COMPOSITE-KEY variant of leavelog's same-GroupID
// dedup — critical for v1.2's correctness because the
// relay path can and will see the same witness record
// multiple times across the gossip fan-out.
func TestSave_DedupesSameTuple(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "witnessed_leaves.json")

	s, _ := Open(path)
	// Same (g, peer) recorded 3 times, with strictly
	// increasing WitnessedAt.
	s.Record(Entry{GroupID: "g_abc", LeaverID: "peerA", WitnessedAt: time.Unix(1, 0).UTC()})
	s.Record(Entry{GroupID: "g_abc", LeaverID: "peerA", WitnessedAt: time.Unix(2, 0).UTC()})
	s.Record(Entry{GroupID: "g_abc", LeaverID: "peerA", WitnessedAt: time.Unix(3, 0).UTC()})
	// Different peer leaving same group — must survive
	// dedup.
	s.Record(Entry{GroupID: "g_abc", LeaverID: "peerB", WitnessedAt: time.Unix(2, 0).UTC()})
	// Different group, same peer — must survive dedup.
	s.Record(Entry{GroupID: "g_xyz", LeaverID: "peerA", WitnessedAt: time.Unix(2, 0).UTC()})

	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	all := s.List()
	if len(all) != 3 {
		t.Fatalf("List len = %d, want 3 (deduped (g_abc, peerA) + (g_abc, peerB) + (g_xyz, peerA)); got %+v", len(all), all)
	}
	// (g_abc, peerA) must be the LATEST (WitnessedAt=3).
	var seenABC, seenABB, seenXYA bool
	for _, e := range all {
		switch {
		case e.GroupID == "g_abc" && e.LeaverID == "peerA":
			seenABC = true
			if e.WitnessedAt.Unix() != 3 {
				t.Errorf("(g_abc, peerA) WitnessedAt = %d, want 3 (most recent)",
					e.WitnessedAt.Unix())
			}
		case e.GroupID == "g_abc" && e.LeaverID == "peerB":
			seenABB = true
		case e.GroupID == "g_xyz" && e.LeaverID == "peerA":
			seenXYA = true
		}
	}
	if !(seenABC && seenABB && seenXYA) {
		t.Errorf("missing rows: seenABC=%v seenABB=%v seenXYA=%v (want all true)",
			seenABC, seenABB, seenXYA)
	}
}

// TestSave_NoopWhenNotDirty pins down the "Save called
// twice, no Record between" path.
func TestSave_NoopWhenNotDirty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "witnessed_leaves.json")
	s, _ := Open(path)
	s.Record(Entry{GroupID: "g_abc", LeaverID: "peerA"})
	if err := s.Save(); err != nil {
		t.Fatalf("first Save: %v", err)
	}
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

// TestSave_TrimsToCap pins down the rolling-window cap.
// 1000 entries is a lot — we record maxWitnessEntries+10
// and assert the oldest 10 are dropped.
func TestSave_TrimsToCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "witnessed_leaves.json")
	s, _ := Open(path)
	for i := 0; i < maxWitnessEntries+10; i++ {
		// Use unique (g, peer) tuples so dedup doesn't
		// kick in.
		s.Record(Entry{
			GroupID:     keyFor(i),
			LeaverID:    "peerA",
			WitnessedAt: time.Unix(int64(i), 0).UTC(),
		})
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	all := s.List()
	if len(all) != maxWitnessEntries {
		t.Errorf("List len = %d, want %d (cap)", len(all), maxWitnessEntries)
	}
	// The OLDEST surviving should be i=10 (we trimmed
	// i=0..9).
	if all[0].GroupID != keyFor(10) {
		t.Errorf("after trim, oldest = %q, want %q", all[0].GroupID, keyFor(10))
	}
}

// TestContains pins down the lookup helper used by
// ApplyRosterUpdate to refuse adding a peer to a group
// when this device has witnessed that peer's leave.
// This is the v1.2 "don't resurrect the dead" rule.
func TestContains(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(filepath.Join(dir, "witnessed_leaves.json"))
	s.Record(Entry{GroupID: "g_abc", LeaverID: "peerA"})

	// (g_abc, peerA) is recorded — should be present.
	if !s.Contains("g_abc", "peerA") {
		t.Error("Contains(g_abc, peerA) = false after Record, want true")
	}
	// Same group, different peer — should NOT be present
	// (this is the critical difference from leavelog,
	// which uses single-key lookup).
	if s.Contains("g_abc", "peerB") {
		t.Error("Contains(g_abc, peerB) = true, want false (different leaver)")
	}
	// Different group, same peer — should NOT be present.
	if s.Contains("g_xyz", "peerA") {
		t.Error("Contains(g_xyz, peerA) = true, want false (different group)")
	}
}

// TestRemove pins down the re-join revocation path.
// When a peer accepts a fresh invite, the (g, self)
// witness entry (if any) is removed so future roster
// updates can land cleanly. Other peers' witness records
// for the same group are preserved.
func TestRemove(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "witnessed_leaves.json")
	s, _ := Open(path)
	s.Record(Entry{GroupID: "g_abc", LeaverID: "peerA"})
	s.Record(Entry{GroupID: "g_abc", LeaverID: "peerB"})
	s.Record(Entry{GroupID: "g_xyz", LeaverID: "peerA"})
	s.Save()

	// Remove (g_abc, peerA).
	if err := s.Remove("g_abc", "peerA"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if s.Contains("g_abc", "peerA") {
		t.Error("after Remove(g_abc, peerA), Contains still true")
	}
	// (g_abc, peerB) must survive (different peer).
	if !s.Contains("g_abc", "peerB") {
		t.Error("Remove of (g_abc, peerA) collateral-removed (g_abc, peerB)")
	}
	// (g_xyz, peerA) must survive (different group).
	if !s.Contains("g_xyz", "peerA") {
		t.Error("Remove of (g_abc, peerA) collateral-removed (g_xyz, peerA)")
	}
	if err := s.Save(); err != nil {
		t.Fatalf("Save after Remove: %v", err)
	}
	// Re-Open, assert (g_abc, peerA) is gone, others
	// remain.
	s2, _ := Open(path)
	if s2.Contains("g_abc", "peerA") {
		t.Error("re-Open still has (g_abc, peerA) on disk")
	}
	if !s2.Contains("g_abc", "peerB") {
		t.Error("re-Open lost (g_abc, peerB)")
	}
	if !s2.Contains("g_xyz", "peerA") {
		t.Error("re-Open lost (g_xyz, peerA)")
	}
}

// TestRemove_NotPresent is the idempotent path: removing
// a (groupID, leaverID) that's not in the log is a
// no-op (returns nil). AcceptGroupInvite calls Remove
// unconditionally; the call must be safe in the
// first-time-join case where witnesslog is empty for
// this group.
func TestRemove_NotPresent(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(filepath.Join(dir, "witnessed_leaves.json"))
	if err := s.Remove("g_never_there", "peerX"); err != nil {
		t.Errorf("Remove of non-present tuple: got %v, want nil", err)
	}
}

// TestRemove_EmptyArgs pins down the caller-bug checks,
// matching Record's empty-input guards.
func TestRemove_EmptyArgs(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(filepath.Join(dir, "witnessed_leaves.json"))
	if err := s.Remove("", "peerA"); err == nil {
		t.Error("Remove(empty GroupID) returned nil, want error")
	}
	if err := s.Remove("g_abc", ""); err == nil {
		t.Error("Remove(empty LeaverID) returned nil, want error")
	}
}

// TestConcurrentRecord is a small smoke test for the
// locking: Record from many goroutines, Save once, all
// entries should land on disk.
func TestConcurrentRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "witnessed_leaves.json")
	s, _ := Open(path)

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(i int) {
			defer wg.Done()
			s.Record(Entry{
				GroupID:     keyFor(i),
				LeaverID:    "peerA",
				WitnessedAt: time.Unix(int64(i), 0).UTC(),
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
func TestParseFailureReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "witnessed_leaves.json")
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

func keyFor(i int) string {
	// Cheap unique GroupID per index. The witnesslog
	// only treats the field as an opaque string.
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
