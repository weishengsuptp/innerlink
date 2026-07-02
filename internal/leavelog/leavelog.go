// Package leavelog is the persistent record of "groups this
// device has left". The purpose is to survive the
// best-effort-broadcast-dropped case in LeaveGroup:
//
//   - Peer A is in group g_xxx with creator C.
//   - A calls LeaveGroup(g_xxx). A's local groups/g_xxx/
//     directory is wiped. A best-effort broadcasts the
//     new roster to remaining members.
//   - If C happens to be offline at that moment, A's
//     broadcast hits no channel — silent drop. C's local
//     roster still has A. The group is "stuck" until C
//     restarts and the next roster push heals it (which
//     doesn't currently happen; see ApplyRosterUpdate).
//   - Pre-fix, the only thing the user could do was
//     manually delete the on-disk members.json, which
//     is gross and error-prone.
//
// v1.1.4 (2026-07-02) introduces this file: when A
// calls LeaveGroup, in addition to the online best-effort
// broadcast, A appends a row to `<DataDir>/leaved_groups.json`.
// On every subsequent handshake, A replays each row as a
// TypeGroupLeaveNotice envelope to whoever they connect
// to. The receiving creator's ApplyLeaveNotice handler
// drops A from their local roster + broadcasts the new
// roster to the remaining members — the same final state
// the online path produces.
//
// Idempotency: ApplyLeaveNotice is a no-op when the
// leaver isn't in the local roster, so a notice replayed
// 5 times by A across 5 handshakes only does the
// roster-removal work once. The creator's leavelog doesn't
// need a symmetric bookkeeping — once they've removed A,
// they're done, even if A sends another notice 6 months
// from now.
//
// This package is intentionally small and parallel to
// internal/selfid: same file-format versioning, same
// tmp+rename atomic Save, same JSON-for-debuggability
// rationale. Read the selfid package doc if you want
// the deeper notes on those choices.
package leavelog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	// CurrentVersion is the on-disk schema version. Bump on
	// incompatible changes; v1 is the only shape we know.
	CurrentVersion = 1

	// maxEntries caps the rolling window. Old entries are
	// dropped FIFO. 100 groups × lifetime of a device is
	// a comfortable margin (real-world leave rate is "a
	// few per year" for most users; this leaves room for
	// the test suite's rapid join/leave cycles too).
	maxEntries = 100
)

// Entry is one row of leaved_groups.json. It records a
// single "this device left group <GroupID> at <LeftAt>"
// fact. The Members field was considered and dropped:
// the creator doesn't need it (they have their own
// roster), and reproducing it on disk would invite
// drift between what A thought the roster was at leave
// time and what C's roster is now. Keep it minimal.
type Entry struct {
	// GroupID is the rendered "g_<64hex>" form, same as
	// everywhere else in the package. The raw form would
	// be slightly cheaper to store but rendered is what
	// the wire format uses (TypeGroupLeaveNotice
	// payload), and replaying to the wire is the only
	// consumer.
	GroupID string `json:"group_id"`
	// LeftAt is wall-clock UTC, RFC3339 in JSON. Used for
	// debugging ("how long ago did A leave?") — not part
	// of any protocol decision.
	LeftAt time.Time `json:"left_at"`
}

// fileFormat is the on-disk JSON shape. The "v" field lets
// us bump the schema later (e.g. add a "reason" hint
// like "kicked" vs "self") without breaking older files.
type fileFormat struct {
	V       int     `json:"v"`
	Entries []Entry `json:"entries"`
}

// Store is the in-memory + on-disk leave log. Safe for
// concurrent use — Save serializes via saveMu, reads via
// mu.RLock. Mirrors internal/selfid.Store's locking model
// for consistency.
type Store struct {
	path string

	mu      sync.RWMutex
	entries []Entry

	saveMu sync.Mutex
	dirty  bool
}

// Open loads leaved_groups.json. A missing file returns
// an empty Store (no error) — the device may have never
// left a group, and the file is created on the first
// Record + Save.
//
// A corrupt or unparseable file is a soft error: we
// return the error AND an empty Store. The caller
// (Node.New) logs the error and continues with no leave
// log — losing one device's worth of "I left X" history
// is far better than refusing to start. The on-disk file
// is left untouched (we don't silently overwrite, in case
// the user wants to recover the data manually).
func Open(path string) (*Store, error) {
	s := &Store{
		path:    path,
		entries: nil,
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return s, fmt.Errorf("leavelog: read %s: %w", path, err)
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		// Soft error: caller decides. See package doc.
		return s, fmt.Errorf("leavelog: parse %s: %w", path, err)
	}
	if f.V != CurrentVersion {
		return s, fmt.Errorf("leavelog: %s: unsupported version %d", path, f.V)
	}
	s.entries = f.Entries
	return s, nil
}

// Record appends a new entry to the log. The entry is
// NOT saved to disk here — call Save() separately so
// multiple Record calls in a single session can be
// batched into one disk write.
//
// An empty GroupID is a caller bug (every group has a
// non-empty rendered ID) and we don't want to record a
// row we'd have to clean up later. Same defensive shape
// as selfid.RecordMigration.
//
// Duplicate inserts (same GroupID recorded twice, e.g.
// a buggy LeaveGroup call from earlier in the session
// before a restart) are silently no-op'd by the dedup
// check in Save. We don't dedup here so the caller can
// see the count go up if they're debugging.
func (s *Store) Record(e Entry) error {
	if e.GroupID == "" {
		return errors.New("leavelog: Record: GroupID is empty")
	}
	if e.LeftAt.IsZero() {
		e.LeftAt = time.Now().UTC()
	}
	s.mu.Lock()
	s.entries = append(s.entries, e)
	s.dirty = true
	s.mu.Unlock()
	return nil
}

// List returns a copy of the current entries. Callers
// (syncLeaveNoticesToPeer) walk this every time a peer
// connects; the size is bounded by maxEntries so the
// cost is negligible.
func (s *Store) List() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Entry, len(s.entries))
	copy(out, s.entries)
	return out
}

// Contains reports whether groupID is in the log. Used
// by ApplyRosterUpdate to skip re-creating a group that
// A has previously left (A's local groups/g_xxx/ is
// already gone — the inbound roster update would
// otherwise blindly write a fresh members.json there,
// re-adding A against A's own will).
func (s *Store) Contains(groupID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.entries {
		if e.GroupID == groupID {
			return true
		}
	}
	return false
}

// Remove drops the entry for groupID from the log, if
// present. Used by AcceptGroupInvite to revoke a
// prior "I left this group" record when the peer is
// re-accepting a fresh invite — the act of joining
// again means the prior leave is moot and the
// ApplyRosterUpdate skip-if-in-leavelog guard must
// release the group for the post-accept roster push
// to land. v1.1.4 (2026-07-02, second hotfix on top
// of the original offline-replay fix).
//
// Idempotent: removing a groupID that isn't in the log
// is a no-op (returns nil). This matters because
// AcceptGroupInvite is called for both first-time
// joins (leavelog empty) and re-joins (leavelog has
// the prior leave entry).
//
// Save is NOT called here — the caller decides when
// to flush, matching the rest of the package's
// Record/Save separation. AcceptGroupInvite's caller
// (the AcceptGroupInvite code path) calls
// leavelog.Save() right after this to ensure the
// removal is durable before the next handshake.
func (s *Store) Remove(groupID string) error {
	if groupID == "" {
		return errors.New("leavelog: Remove: GroupID is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	filtered := s.entries[:0]
	for _, e := range s.entries {
		if e.GroupID == groupID {
			found = true
			continue
		}
		filtered = append(filtered, e)
	}
	if !found {
		return nil
	}
	// Re-slice into a fresh allocation to avoid
	// retaining a backing array that's still the
	// full length of s.entries (with a tail of
	// out-of-bounds slots that the next Record call
	// would re-overwrite).
	trimmed := make([]Entry, len(filtered))
	copy(trimmed, filtered)
	s.entries = trimmed
	s.dirty = true
	return nil
}

// Save writes the in-memory entries to disk using the
// tmp+rename pattern (matches internal/selfid.Save,
// internal/roster.Save, etc.). Concurrent calls
// serialize via saveMu so we never interleave two
// rename()s on the same path.
//
// Dedup: if the same GroupID was recorded N times in a
// session, only the most recent row survives. This
// matters if a buggy caller double-records, but also for
// the natural "leave, rejoin, leave" cycle — we want
// the most recent leave event to win so the creator
// drops us (the rejoin's roster sync and the subsequent
// leave both get reflected as a single "currently
// outside this group" state on the next replay).
//
// Trim: if total entries exceed maxEntries, drop the
// oldest. The trim is FIFO on insertion order; the most
// recent maxEntries stay. This keeps the file bounded
// over a long-lived device.
func (s *Store) Save() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	// Dedup by GroupID, keep the latest. Walk in reverse
	// so the first occurrence of a GroupID we see is
	// its latest; everything older gets dropped.
	seen := make(map[string]struct{}, len(s.entries))
	deduped := make([]Entry, 0, len(s.entries))
	for i := len(s.entries) - 1; i >= 0; i-- {
		e := s.entries[i]
		if _, ok := seen[e.GroupID]; ok {
			continue
		}
		seen[e.GroupID] = struct{}{}
		deduped = append(deduped, e)
	}
	// deduped is now in reverse order. Reverse back so
	// the file reads in chronological order (helps the
	// `cat` debugging story).
	for i, j := 0, len(deduped)-1; i < j; i, j = i+1, j-1 {
		deduped[i], deduped[j] = deduped[j], deduped[i]
	}
	// Trim to maxEntries (FIFO from oldest).
	if len(deduped) > maxEntries {
		deduped = deduped[len(deduped)-maxEntries:]
	}
	s.entries = deduped
	s.dirty = false
	toWrite := deduped
	s.mu.Unlock()

	// v1.1.4 (2026-07-02) bug-pattern carryover from
	// internal/selfid.Save: ensure the parent directory
	// exists before WriteFile. The data dir is normally
	// created by layout.Ensure() in Node.New, but
	// test fixtures and edge-case restarts can call
	// Save before that, and WriteFile doesn't create
	// parent dirs (it just ENOENTs). Cheaper to guard
	// here than to assume every caller got the order
	// right.
	if dir := filepath.Dir(s.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("leavelog: mkdir %s: %w", dir, err)
		}
	}

	f := fileFormat{V: CurrentVersion, Entries: toWrite}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("leavelog: marshal: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("leavelog: write tmp: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("leavelog: rename: %w", err)
	}
	return nil
}
