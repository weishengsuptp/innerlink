// Package selfid tracks the "self identity history" of an
// innerlink device. A "device" is the SM2 key pair stored in
// device.key; when that file is wiped (manual delete, re-paste
// of the EXE, OS reinstall, etc.), the next launch generates a
// fresh SM2 key with a new PeerID. The OLD PeerID is then
// stranded in:
//
//   - every peer's roster.json (marked reset=true, but kept
//     on disk forever per the v1.1 reset-sticky policy)
//   - every group members.json we were a member of
//   - our own aliases.json keyed by the old PeerID
//   - chat.enc records we sent (From field is the old PeerID)
//
// v1.1.4 (2026-07-02) hotfix: this package exists so Node.Start
// can read the prior peerID(s), claim ownership of any group /
// alias / roster references that still point at them, and atomically
// replace them with the NEW peerID. The on-disk file is
// `<DataDir>/self_history.json`, JSON for human-debuggability
// (the same rationale as roster.json — `cat` works without a tool).
//
// Design contract:
//
//   - History file is APPEND-ONLY from the writer's perspective
//     (RecordMigration appends). The Claim caller is allowed to
//     clear or trim entries, but in practice we keep them around
//     for a 7-day rolling window so gossip dedup can still match
//     "incoming entry with old self peerID" cases.
//   - History is never written from anywhere except this package.
//   - Load is non-destructive: a parse error returns (empty
//     history, error) — the caller decides whether to abort or
//     to log + continue with no claim (the latter is what
//     Node.Start does; better to start up with a small risk of
//     a stale alias than to refuse to start).
//   - Save uses the same tmp+rename atomic-write pattern as
//     internal/roster and internal/alias.
package selfid

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

	// maxEntries is the rolling-window cap. Older entries are
	// dropped FIFO. 7 days of daily wipe+reinstalls gives a
	// comfortable margin.
	maxEntries = 10

	// MaxAge is how long an entry stays "claimable" — past this
	// window the peerID is considered dead and a wipe that
	// wants to re-claim it just makes a fresh history. Currently
	// unused (we use the rolling-window cap instead) but kept
	// here so callers can read the constant.
	MaxAge = 7 * 24 * time.Hour
)

// Trigger enumerates why a peerID migration was recorded.
// The value lands in the on-disk file (for humans + tests) and
// in the [SYNC] log line. Adding a new value is backward-
// compatible: readers fall back to "unknown" for unknown strings.
type Trigger string

const (
	// TriggerFreshInstall is recorded when the device had
	// NO prior peerID (no device.key, no history). old_peer_id
	// is "".
	TriggerFreshInstall Trigger = "fresh_install"

	// TriggerWipeReinstall is recorded when the device had a
	// prior peerID in history. Most common case: user deleted
	// the .innerlink folder and re-pasted the EXE.
	TriggerWipeReinstall Trigger = "wipe_and_reinstall"

	// TriggerManualReset is reserved for a future public API
	// (e.g. cmd/innerlink reset-identity) that rotates the
	// device key on demand. Not used yet; documented so the
	// schema doesn't need a migration when we add it.
	TriggerManualReset Trigger = "manual_reset"
)

// Entry is one row of the self_history migration log.
type Entry struct {
	// OldPeerID is the peerID we're moving AWAY from. Empty
	// for TriggerFreshInstall (no prior identity).
	OldPeerID string `json:"old_peer_id,omitempty"`
	// NewPeerID is the peerID we're moving TO. Always set.
	NewPeerID string `json:"new_peer_id"`
	// SwitchedAt is the wall-clock time the migration was
	// recorded. UTC, RFC3339 in JSON.
	SwitchedAt time.Time `json:"switched_at"`
	// Trigger is one of the Trigger* constants. Unknown
	// values round-trip as the raw string.
	Trigger Trigger `json:"trigger"`
}

// fileFormat is the on-disk JSON shape. The "v" field lets
// us bump the schema later (e.g. add a "source" hint) without
// breaking older files.
type fileFormat struct {
	V       int     `json:"v"`
	Entries []Entry `json:"entries"`
}

// Store is the in-memory + on-disk self history. Safe for
// concurrent use — Save serializes via saveMu, reads via
// mu.RLock.
type Store struct {
	path string

	mu      sync.RWMutex
	entries []Entry

	saveMu sync.Mutex
	dirty  bool
}

// Open loads self_history.json. A missing file returns an
// empty Store (no error) — the very first launch has no
// prior identity to claim, and the file is created on the
// first RecordMigration + Save.
//
// A corrupt or unparseable file is a soft error: we return
// the error AND an empty Store. The caller (Node.Start)
// logs the error and continues with no claim — losing one
// wipe-cycle of self-claim is far better than refusing to
// start. The on-disk file is left untouched (we don't
// silently overwrite, in case the user wants to recover
// the data manually).
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
		return s, fmt.Errorf("selfid: read %s: %w", path, err)
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		// Soft error: caller decides. See package doc.
		return s, fmt.Errorf("selfid: parse %s: %w", path, err)
	}
	if f.V != CurrentVersion {
		return s, fmt.Errorf("selfid: %s: unsupported version %d", path, f.V)
	}
	s.entries = f.Entries
	return s, nil
}

// RecordMigration appends a new entry to the history. The
// caller is responsible for filling OldPeerID (or "" for
// fresh install), NewPeerID, SwitchedAt, and Trigger. The
// entry is NOT saved to disk here — call Save() separately
// so multiple migrations in a single session can be batched.
//
// If NewPeerID is empty, RecordMigration is a no-op (and
// logs a warning via the returned error). An empty
// NewPeerID is a caller bug — peerIDs are always 32 hex
// chars — and we don't want to record a row we'd have to
// clean up later.
func (s *Store) RecordMigration(e Entry) error {
	if e.NewPeerID == "" {
		return errors.New("selfid: RecordMigration: NewPeerID is empty")
	}
	if e.SwitchedAt.IsZero() {
		e.SwitchedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, e)
	// Trim to maxEntries: keep the most recent N. Older
	// ones are dropped. We don't bother preserving them
	// anywhere because the rolling window is exactly the
	// claim-relevant history.
	if len(s.entries) > maxEntries {
		// Drop from the FRONT, not the back, so we keep
		// the most recent (newest) entries — the ones
		// most likely to be referenced by other peers'
		// gossip and group state.
		drop := len(s.entries) - maxEntries
		s.entries = s.entries[drop:]
	}
	s.dirty = true
	return nil
}

// OldPeerIDs returns the list of peerIDs that should be
// migrated to the current self. The list excludes entries
// where OldPeerID is empty (fresh installs) and where
// NewPeerID equals OldPeerID (defensive — should never
// happen, but filtering makes the claim code safer).
//
// Returned slice is a fresh copy — callers may mutate it.
func (s *Store) OldPeerIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.entries) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.entries))
	for _, e := range s.entries {
		if e.OldPeerID == "" {
			continue
		}
		if e.OldPeerID == e.NewPeerID {
			continue
		}
		out = append(out, e.OldPeerID)
	}
	return out
}

// Latest returns the most recent entry, or (zero Entry,
// false) if the history is empty. Used by Node.Start to
// decide whether to record a TriggerFreshInstall on first
// launch (Latest is empty) or a TriggerWipeReinstall
// (Latest.OldPeerID non-empty).
func (s *Store) Latest() (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.entries) == 0 {
		return Entry{}, false
	}
	return s.entries[len(s.entries)-1], true
}

// Save writes the current state to disk atomically (tmp +
// rename). No-op if the file is clean. Returns nil on
// success.
//
// Mirrors internal/roster.Store.Save: snapshot under the
// read lock, marshal outside the lock, atomic rename.
func (s *Store) Save() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.RLock()
	if !s.dirty {
		s.mu.RUnlock()
		return nil
	}
	snapshot := make([]Entry, len(s.entries))
	copy(snapshot, s.entries)
	s.mu.RUnlock()

	f := fileFormat{
		V:       CurrentVersion,
		Entries: snapshot,
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("selfid: marshal: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("selfid: mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "self_history-*.json.tmp")
	if err != nil {
		return fmt.Errorf("selfid: create tmp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Clean up on any error path.
		_ = os.Remove(tmpName)
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("selfid: write tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("selfid: close tmp: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("selfid: rename: %w", err)
	}
	s.mu.Lock()
	s.dirty = false
	s.mu.Unlock()
	return nil
}

// Path returns the on-disk path. Used by Node for logging
// during claim — the user wants to know which file the
// claim history lives in.
func (s *Store) Path() string {
	return s.path
}
