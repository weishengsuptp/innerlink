// Package witnesslog is the persistent record of "leave
// notices I have witnessed for OTHER peers in OTHER groups
// where I'm still a member". It is the receiver-side
// companion to internal/leavelog and is what makes v1.2's
// "ripple propagation" work without the leaver having to
// come back online to deliver their notice themselves.
//
// v1.1.4 (2026-07-02) introduced leavelog: when A calls
// LeaveGroup, A persists the leave and replays it on every
// subsequent handshake so the creator (and any other peer
// A can reach) eventually drops A. v1.1.4 has a gap: if A
// is offline and never reconnects (think: A leaves and
// shuts the laptop), the only peers who can be reached
// are those A happened to be on a channel with at leave
// time. A peer who was offline at that moment — and stays
// offline longer than the channel lifetime — never gets
// the notice through A's own replay path.
//
// v1.2 (2026-07-06) closes this with witnesslog. When B
// receives A's leave notice, B records it to B's own
// witnesslog. B's handshake-time leave pre-sync now also
// pushes B's witnesslog, so B becomes a relay: B hands A's
// notice to C the next time B and C connect, even if A
// is long gone. C in turn records it and pushes it to D.
// The gossip ripples outward; offline peers are caught up
// the moment they handshake with ANY online peer who has
// seen the notice.
//
// Idempotency: a notice may be relayed by many peers. The
// receiving side's ApplyLeaveNotice is a no-op when the
// leaver isn't in the local roster (so re-relays cost a
// packet but no semantic work). The witnesslog itself
// does NOT dedup at write time — each (GroupID, LeaverID)
// pair is recorded at most once per Save (dedup keeps the
// latest), but a single in-process session that processes
// the same relay 3 times will record 3 entries; the dedup
// happens at Save time. Trim FIFO (maxWitnessEntries)
// bounds the file size over long-lived devices.
//
// IMPORTANT (semantic split from leavelog):
//
//   leavelog  = "groups THIS DEVICE has left"  (self POV)
//   witnesslog = "leaves THIS DEVICE has witnessed for
//                  OTHER peers"  (observer POV)
//
// v1.1.4 baseline had a hotfix attempt (commit 03b7b68)
// that tried to reuse leavelog for both — it failed
// because ApplyRosterUpdate's "leavelog.Contains = I left"
// guard then mis-fired when the witness entry was the
// only record for a group the device was still in. v1.2
// keeps these two stores physically separate (different
// files, different in-memory Store types) so neither
// caller can confuse the other.
//
// This package is intentionally parallel to
// internal/leavelog: same file-format versioning, same
// tmp+rename atomic Save, same JSON-for-debuggability
// rationale. Read the leavelog package doc for the
// deeper notes on those choices.
package witnesslog

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

	// maxWitnessEntries caps the rolling window. 1000 is
	// roughly 50 "moderately-leavy" groups (50 members,
	// half leave = 25 witness entries per group) — far
	// more than innerlink's expected workload. Real
	// per-device count is "a handful" for typical users.
	maxWitnessEntries = 1000
)

// Entry is one row of witnessed_leaves.json. It records
// a single "I saw peer <LeaverID> leave group <GroupID>
// at <WitnessedAt>" fact.
//
// LeaverID is explicit (unlike leavelog.Entry, which
// implicitly has LeaverID = self). Storing it is what
// makes relay work: the witnesser's handshake pre-sync
// uses LeaverID when constructing the LeaveNotice
// envelope, and the receiver's ApplyLeaveNotice uses it
// to find and remove the leaver from the local roster.
type Entry struct {
	// GroupID is the rendered "g_<64hex>" form, same as
	// everywhere else in the package.
	GroupID string `json:"group_id"`
	// LeaverID is the peerID of the peer who left. Stored
	// as the rendered hex form (32 chars) so the wire
	// format and the on-disk form match — no
	// hex<->bytes round-trip needed at replay time.
	LeaverID string `json:"leaver_id"`
	// WitnessedAt is the wall-clock UTC moment the
	// witnessing peer observed the leave notice
	// (i.e. the time ApplyLeaveNotice processed it, not
	// the time A actually self-destructed). Used for
	// debugging and FIFO ordering in the trim path —
	// not part of any protocol decision.
	WitnessedAt time.Time `json:"witnessed_at"`
}

// fileFormat is the on-disk JSON shape. The "v" field lets
// us bump the schema later without breaking older files.
type fileFormat struct {
	V       int     `json:"v"`
	Entries []Entry `json:"entries"`
}

// Store is the in-memory + on-disk witness log. Safe for
// concurrent use — Save serializes via saveMu, reads via
// mu.RLock. Mirrors internal/leavelog.Store's locking
// model for consistency.
type Store struct {
	path string

	mu      sync.RWMutex
	entries []Entry

	saveMu sync.Mutex
	dirty  bool
}

// Open loads witnessed_leaves.json. A missing file
// returns an empty Store (no error) — the device may have
// never witnessed a leave, and the file is created on
// the first Record + Save.
//
// A corrupt or unparseable file is a soft error: we
// return the error AND an empty Store. The caller
// (Node.New) logs the error and continues with no
// witness log — losing one device's worth of witnessed
// leaves is far better than refusing to start. The
// on-disk file is left untouched (we don't silently
// overwrite, in case the user wants to recover the
// data manually).
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
		return s, fmt.Errorf("witnesslog: read %s: %w", path, err)
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		// Soft error: caller decides. See package doc.
		return s, fmt.Errorf("witnesslog: parse %s: %w", path, err)
	}
	if f.V != CurrentVersion {
		return s, fmt.Errorf("witnesslog: %s: unsupported version %d", path, f.V)
	}
	s.entries = f.Entries
	return s, nil
}

// Record appends a new entry to the log. The entry is
// NOT saved to disk here — call Save() separately so
// multiple Record calls in a single session can be
// batched into one disk write.
//
// An empty GroupID or LeaverID is a caller bug and we
// don't want to record a row we'd have to clean up
// later. Same defensive shape as leavelog.Record.
func (s *Store) Record(e Entry) error {
	if e.GroupID == "" {
		return errors.New("witnesslog: Record: GroupID is empty")
	}
	if e.LeaverID == "" {
		return errors.New("witnesslog: Record: LeaverID is empty")
	}
	if e.WitnessedAt.IsZero() {
		e.WitnessedAt = time.Now().UTC()
	}
	s.mu.Lock()
	s.entries = append(s.entries, e)
	s.dirty = true
	s.mu.Unlock()
	return nil
}

// Remove drops entries matching (groupID, leaverID) from
// the log. Used by AcceptGroupInvite to revoke a prior
// witness when a peer is re-accepting a fresh invite —
// the act of joining again doesn't invalidate "I saw
// someone leave", but it DOES mean the re-invited peer
// should clear the self-referential witness record (if
// any) so the rejoin can land cleanly. Other peers'
// witness records against this group are preserved —
// they're observational facts that don't go stale just
// because the group membership shifted.
//
// Idempotent: removing a (groupID, leaverID) that isn't
// in the log is a no-op (returns nil).
//
// Save is NOT called here — the caller decides when to
// flush, matching the rest of the package's Record/Save
// separation.
func (s *Store) Remove(groupID, leaverID string) error {
	if groupID == "" {
		return errors.New("witnesslog: Remove: GroupID is empty")
	}
	if leaverID == "" {
		return errors.New("witnesslog: Remove: LeaverID is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	filtered := s.entries[:0]
	dropped := false
	for _, e := range s.entries {
		if e.GroupID == groupID && e.LeaverID == leaverID {
			dropped = true
			continue
		}
		filtered = append(filtered, e)
	}
	if !dropped {
		return nil
	}
	// Re-slice into a fresh allocation to avoid
	// retaining a backing array (same reasoning as
	// leavelog.Remove).
	trimmed := make([]Entry, len(filtered))
	copy(trimmed, filtered)
	s.entries = trimmed
	s.dirty = true
	return nil
}

// List returns a copy of the current entries. Callers
// (syncLeaveNoticesToPeer Part 2) walk this every time
// a peer connects; the size is bounded by
// maxWitnessEntries so the cost is negligible.
func (s *Store) List() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Entry, len(s.entries))
	copy(out, s.entries)
	return out
}

// Contains reports whether (groupID, leaverID) is in
// the log. Used by ApplyRosterUpdate to refuse adding
// a peer to a group when this device has witnessed
// that peer's leave — that's the v1.2 "don't resurrect
// the dead" rule.
//
// O(n) linear scan, but n is bounded by maxWitnessEntries
// (1000) and this is only called once per inbound roster
// update per group, so the cost is fine.
func (s *Store) Contains(groupID, leaverID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.entries {
		if e.GroupID == groupID && e.LeaverID == leaverID {
			return true
		}
	}
	return false
}

// Save writes the in-memory entries to disk using the
// tmp+rename pattern (matches internal/leavelog.Save,
// internal/selfid.Save, internal/roster.Save, etc.).
// Concurrent calls serialize via saveMu so we never
// interleave two rename()s on the same path.
//
// Dedup: if the same (GroupID, LeaverID) was recorded
// N times in a session, only the most recent row
// survives. This matters if a buggy caller
// double-records, but also for the natural "I see the
// same leave relayed 5 times" case — we want the most
// recent observation to win.
//
// Trim: if total entries exceed maxWitnessEntries, drop
// the oldest. The trim is FIFO on insertion order; the
// most recent maxWitnessEntries stay. This keeps the
// file bounded over a long-lived device.
func (s *Store) Save() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	// Dedup by (GroupID, LeaverID), keep the latest.
	// Walk in reverse so the first occurrence of a
	// (group, peer) pair we see is its latest;
	// everything older gets dropped.
	type key struct{ g, l string }
	seen := make(map[key]struct{}, len(s.entries))
	deduped := make([]Entry, 0, len(s.entries))
	for i := len(s.entries) - 1; i >= 0; i-- {
		e := s.entries[i]
		k := key{e.GroupID, e.LeaverID}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		deduped = append(deduped, e)
	}
	// deduped is now in reverse order. Reverse back so
	// the file reads in chronological order (helps the
	// `cat` debugging story).
	for i, j := 0, len(deduped)-1; i < j; i, j = i+1, j-1 {
		deduped[i], deduped[j] = deduped[j], deduped[i]
	}
	// Trim to maxWitnessEntries (FIFO from oldest).
	if len(deduped) > maxWitnessEntries {
		deduped = deduped[len(deduped)-maxWitnessEntries:]
	}
	s.entries = deduped
	s.dirty = false
	toWrite := deduped
	s.mu.Unlock()

	// Ensure the parent directory exists before
	// WriteFile. The data dir is normally created by
	// layout.Ensure() in Node.New, but test fixtures
	// and edge-case restarts can call Save before
	// that, and WriteFile doesn't create parent dirs
	// (it just ENOENTs). Cheaper to guard here than to
	// assume every caller got the order right.
	if dir := filepath.Dir(s.path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("witnesslog: mkdir %s: %w", dir, err)
		}
	}

	tmp := s.path + ".tmp"
	data, err := json.MarshalIndent(fileFormat{
		V:       CurrentVersion,
		Entries: toWrite,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("witnesslog: marshal: %w", err)
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("witnesslog: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("witnesslog: rename %s -> %s: %w", tmp, s.path, err)
	}
	return nil
}
