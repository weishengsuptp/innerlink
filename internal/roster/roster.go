// Package roster is the distributed peer directory for innerlink.
//
// Mental model (v0.5+): each innerlink instance maintains a local
// "phone book" of every peer it has ever heard about on the LAN.
// Phone books are kept loosely consistent across instances via
// a gossip protocol — when two peers establish a channel, they
// exchange their books; new entries discovered through gossip
// are merged in. The result is that within a small LAN
// (3-50 nodes), every instance eventually has the same set of
// "peers I've heard about" without needing a central server.
//
// What goes in the book (synced across the network):
//
//   - peerID      : the 16-byte SM3-derived identifier (hex)
//   - hostname    : the device's self-declared name
//   - alias       : the device's self-chosen display name,
//                   broadcast via M5 RosterSync (2026-06-24+)
//                   and stored in <data-dir>/alias.txt. Owner
//                   edits it, others only view.
//   - addrs       : the IP:port pairs the peer is reachable at
//                   (a multi-NIC machine publishes several)
//   - first_seen  : when we first heard about this peer
//   - source      : which peer told us about this one (trust chain)
//
// What does NOT go in the book (kept local):
//
//   - reset       : one-shot dedup marker. When we see a NEW
//                   (peerID, IP, hostname) come online that
//                   collides on (IP, hostname) with an old
//                   OFFLINE entry having a different peerID,
//                   the old entry is "the previous host that
//                   lived at this IP+hostname before being
//                   reinstalled". We mark the old entry
//                   reset=true once and hide it from the UI
//                   forever. Reset is sticky: even if the old
//                   peerID comes back, we keep it hidden so the
//                   state doesn't flicker (the user said:
//                   "一次性, 防止状态反复").
//   - presence    : whether a peer is currently online is a
//                   real-time local observation derived from
//                   the active channel state. You can't trust
//                   "B told me C is online" because by the time
//                   you receive the message, C may have left.
//                   Presence is re-checked by attempting a
//                   handshake on every roster entry.
package roster

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// CurrentVersion is the on-disk file format version. Bump
// this if the JSON layout changes incompatibly.
//
//   v1 (initial)         : peer_id / hostname / addrs / first_seen / last_seen / source
//   v2 (2026-06-24+)     : + alias (broadcast self-display-name from M5 gossip)
//                          + reset  (one-shot dedup marker — see MarkReset / MergeFromGossip)
const CurrentVersion = 2

// peerIDSize is the length of a PeerID in hex chars.
// Kept identical to identity.PeerIDSize (32) — we
// don't import identity to avoid a cycle.
const peerIDSize = 32

// Entry is one row of the roster — what we know about
// a single peer. The fields are intentionally minimal:
// this is the public "phone book" content, and the
// gossip protocol exchanges these.
type Entry struct {
	// PeerID is the 32-char lowercase hex of the peer's
	// 16-byte SM3-derived identity. The map key in Store.
	PeerID string `json:"peer_id"`
	// Hostname is the device's self-declared name. May
	// change over time (DHCP, user rename); the latest
	// gossip wins.
	Hostname string `json:"hostname"`
	// Alias is the device's self-chosen display name,
	// broadcast over M5 RosterSync (added in v2). Empty
	// when the peer hasn't set one yet. Synced across
	// the LAN via gossip; the latest write wins.
	Alias string `json:"alias,omitempty"`
	// Addrs is the set of IP:port pairs this peer is
	// reachable at. A machine with multiple NICs
	// publishes multiple. Order is not significant.
	Addrs []string `json:"addrs"`
	// FirstSeen is when we first heard about this peer.
	// Set once, never updated.
	FirstSeen time.Time `json:"first_seen"`
	// LastSeen is the most recent time we observed this
	// peer (handshake, channel ready, gossip). Updated
	// on every contact. Not synced — derived locally.
	LastSeen time.Time `json:"last_seen"`
	// Source is the peerID of the node that told us
	// about this entry. Empty when we discovered the
	// peer directly (UDP discovery or direct dial).
	// Used for trust-chain debugging, not enforced.
	Source string `json:"source,omitempty"`
	// Reset is the one-shot dedup marker. When set, the
	// UI hides this entry forever, even if the underlying
	// peerID comes back. Set by MergeFromGossip when an
	// online (peerID=A, IP=X, hostname=foo) collides on
	// (IP=X, hostname=foo) with an existing offline
	// entry (peerID=B) — meaning A is the new install
	// at the same address, B is the ghost of the old
	// device. Not synced; purely local.
	Reset bool `json:"reset,omitempty"`
}

// fileFormat is the on-disk JSON shape. The "v" field
// lets us bump the schema later without breaking older
// files.
type fileFormat struct {
	V      int             `json:"v"`
	Entry  map[string]Entry `json:"entries"`
}

// Store is the in-memory + on-disk roster. All exported
// methods are safe for concurrent use. The on-disk
// representation is JSON for human-debuggability — a
// user can `cat .innerlink/roster.json` and see who's
// in their network.
type Store struct {
	path string

	mu      sync.RWMutex // protects m
	m       map[string]Entry

	saveMu sync.Mutex // serializes Save()
	dirty  bool       // m has changes not yet on disk
}

// Open loads the roster from path. If the file does
// not exist, Open returns an empty Store ready to
// accept Add/Remove — the file is created on the
// first Save, not eagerly. Same policy as alias
// and storage: no side effects until the user
// actually does something.
//
// A corrupt or unparseable file is a hard error —
// silently starting with an empty book would be the
// "we lost your data" failure mode.
//
// v1 → v2 migration: the schema added `alias` and
// `reset` fields in v2. Both default to their zero
// value (alias="", reset=false) when reading v1
// data, so no special per-entry transform is needed;
// we just bump V on the next Save.
func Open(path string) (*Store, error) {
	s := &Store{
		path: path,
		m:    make(map[string]Entry),
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, fmt.Errorf("roster: read %s: %w", path, err)
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("roster: parse %s: %w", path, err)
	}
	if f.V != 1 && f.V != CurrentVersion {
		return nil, fmt.Errorf("roster: %s: unsupported version %d", path, f.V)
	}
	if f.Entry != nil {
		s.m = f.Entry
	}
	return s, nil
}

// Add inserts or updates an entry. Empty peerID is an
// error (caller bug). If the entry already exists, the
// hostname, alias, and addrs are refreshed, first_seen
// is kept, last_seen is updated to now, source is
// updated only if the new source is non-empty, and
// Reset is sticky (existing Reset=true is preserved).
//
// Dedup scan (2026-06-24+): when we Add an entry with
// a hostname + at least one addr, scan the existing
// table for any (hostname, IP-overlap) entry with a
// DIFFERENT peerID. Mark those Reset=true once. This
// catches the "ghost of the previous install at this
// address" case whether the new entry came from gossip
// OR from the local self-entry Add at startup — the
// latter was the source of the bug where a regenerated
// device.key left the old self-entry visible as a peer
// in the user's own UI.
//
// Returns true if the entry is new (didn't exist
// before). Callers use this to decide whether to push
// the new entry to connected peers for gossip.
func (s *Store) Add(e Entry) (added bool, err error) {
	if !validPeerID(e.PeerID) {
		return false, fmt.Errorf("roster: peer id must be %d lowercase hex chars", peerIDSize)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, ok := s.m[e.PeerID]
	if ok {
		// Merge: keep first_seen; refresh hostname/alias/addrs;
		// preserve Reset (sticky once marked).
		e.FirstSeen = existing.FirstSeen
		if existing.Reset {
			e.Reset = true
		}
	}
	if e.FirstSeen.IsZero() {
		e.FirstSeen = time.Now().UTC()
	}
	if e.LastSeen.IsZero() {
		e.LastSeen = time.Now().UTC()
	}
	// Dedup scan: same as MergeFromGossip. We run it
	// from BOTH Add and MergeFromGossip so that ghosts
	// are caught no matter which path learns about the
	// new peerID first. The scan only marks ghosts
	// reset; it never overwrites or deletes them, so
	// double-scanning is harmless.
	if len(e.Addrs) > 0 && e.Hostname != "" {
		for existingPID, ghost := range s.m {
			if existingPID == e.PeerID {
				continue
			}
			if ghost.Reset {
				continue
			}
			if ghost.Hostname != e.Hostname {
				continue
			}
			if !addrsOverlap(ghost.Addrs, e.Addrs) {
				continue
			}
			ghost.Reset = true
			s.m[existingPID] = ghost
			s.dirty = true
		}
	}
	s.m[e.PeerID] = e
	s.dirty = true
	return !ok, nil
}

// Remove deletes the entry for peerID. Returns
// ErrNotFound if no such entry.
var ErrNotFound = errors.New("roster: peer not found")

func (s *Store) Remove(peerID string) error {
	if !validPeerID(peerID) {
		return fmt.Errorf("roster: peer id must be %d lowercase hex chars", peerIDSize)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[peerID]; !ok {
		return ErrNotFound
	}
	delete(s.m, peerID)
	s.dirty = true
	return nil
}

// Get returns the entry for peerID, or ErrNotFound.
func (s *Store) Get(peerID string) (Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.m[peerID]
	if !ok {
		return Entry{}, ErrNotFound
	}
	return e, nil
}

// Touch updates LastSeen for an existing entry. No-op
// if the entry doesn't exist (the caller probably has
// a race with gossip eviction; the next gossip will
// re-add it).
func (s *Store) Touch(peerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[peerID]
	if !ok {
		return
	}
	e.LastSeen = time.Now().UTC()
	s.m[peerID] = e
	s.dirty = true
}

// List returns a snapshot of all entries, sorted by
// PeerID (so the on-disk file and the gossip message
// have a stable order — easier to diff in tests and
// log analysis).
//
// List INCLUDES reset entries — callers that want
// the user-facing subset should use ListActive().
// Internal use (gossip payload, save) keeps the full
// table so the on-disk file is the source of truth
// and reset state survives restarts.
func (s *Store) List() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Entry, 0, len(s.m))
	for _, e := range s.m {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].PeerID < out[j].PeerID
	})
	return out
}

// ListActive is the UI-facing view: every entry
// except those marked Reset=true. Used by PeerInfo
// aggregation in pkg/node so the user never sees a
// ghost of the previous install at the same IP.
//
// Reset is sticky across restarts (lives in
// roster.json), so this filter is stable too — once
// an entry drops out of ListActive, it stays out.
func (s *Store) ListActive() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Entry, 0, len(s.m))
	for _, e := range s.m {
		if e.Reset {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].PeerID < out[j].PeerID
	})
	return out
}

// MarkReset sets Reset=true on the entry for peerID.
// No-op if the entry doesn't exist or is already
// reset. Idempotent (the whole point — "一次性,
// 防止状态反复"). The dirty flag is set so the
// change reaches disk on the next Save.
func (s *Store) MarkReset(peerID string) error {
	if !validPeerID(peerID) {
		return fmt.Errorf("roster: peer id must be %d lowercase hex chars", peerIDSize)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[peerID]
	if !ok {
		return ErrNotFound
	}
	if e.Reset {
		return nil
	}
	e.Reset = true
	s.m[peerID] = e
	s.dirty = true
	return nil
}

// MergeFromGossip is the entry point for the gossip
// protocol. It applies a remote peer's roster view
// into our local one.
//
// Existing entries — UPDATE RULE (2026-06-24+):
//   - alias is the peer's OWN self-display-name. The
//     owner is the authoritative source, so gossip
//     always updates alias (even on existing entries).
//     This is what makes "改了别名要广播给其他客户端"
//     work: when A changes its alias, A's next
//     RosterSync updates B's local alias for A.
//   - hostname / addrs stay "local direct observation
//     is authoritative" — gossip can be stale, and a
//     direct channel gives fresher info. (Pre-2026-06-24
//     behavior.) Future: same trust rule as alias if a
//     use case shows up.
//
// New entries are added.
//
// Dedup (2026-06-24+): before adding a new entry, scan
// the existing map for any (IP, hostname) collision
// with a different peerID. The existing entry is the
// "previous install at this address"; we mark it
// Reset=true once. The new entry is then added
// normally. The reset marker is sticky.
//
// Returns the list of peerIDs that were newly added —
// the caller uses this to decide whether to schedule
// a dial to those peers (presence probe). Reset
// victims are NOT included.
func (s *Store) MergeFromGossip(remote []Entry) (newlyAdded []string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range remote {
		if len(e.PeerID) != peerIDSize {
			// Skip malformed gossip entries rather than
			// failing the whole merge. A single bad
			// entry from a misbehaving peer should not
			// prevent the rest from being adopted.
			continue
		}
		if existing, exists := s.m[e.PeerID]; exists {
			// Same peerID: refresh only the alias.
			// Hostname/addrs are locally authoritative.
			// We always apply the gossip's alias, even
			// if it equals what we already have (this
			// also handles the "clear" path: a gossip
			// entry with empty alias — omitempty on the
			// wire means a cleared alias doesn't reach
			// us, so this branch is mostly for explicit
			// empty strings if a future wire change
			// adds them).
			if existing.Alias != e.Alias {
				existing.Alias = e.Alias
				s.m[e.PeerID] = existing
				s.dirty = true
			}
			continue
		}
		// New entry. Dedup scan: any existing entry
		// with a matching (IP, hostname) but DIFFERENT
		// peerID is a "ghost of the previous install"
		// — mark it reset.
		if len(e.Addrs) > 0 && e.Hostname != "" {
			for existingPID, ghost := range s.m {
				if existingPID == e.PeerID {
					continue
				}
				if ghost.Reset {
					continue
				}
				if ghost.Hostname != e.Hostname {
					continue
				}
				if !addrsOverlap(ghost.Addrs, e.Addrs) {
					continue
				}
				ghost.Reset = true
				s.m[existingPID] = ghost
				s.dirty = true
			}
		}
		if e.FirstSeen.IsZero() {
			e.FirstSeen = time.Now().UTC()
		}
		if e.LastSeen.IsZero() {
			e.LastSeen = time.Now().UTC()
		}
		s.m[e.PeerID] = e
		s.dirty = true
		newlyAdded = append(newlyAdded, e.PeerID)
	}
	sort.Strings(newlyAdded)
	return newlyAdded, nil
}

// addrsOverlap returns true if two addr lists share
// at least one IP:port. Used by MergeFromGossip to
// decide whether two roster entries likely describe
// the same physical host (same IP). Order-insensitive;
// nil/empty addr lists never overlap.
func addrsOverlap(a, b []string) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	seen := make(map[string]struct{}, len(a))
	for _, x := range a {
		seen[x] = struct{}{}
	}
	for _, y := range b {
		if _, ok := seen[y]; ok {
			return true
		}
	}
	return false
}

// Save writes the current state to disk. Idempotent.
// Uses atomic write (tmp + rename) so a crash mid-write
// doesn't corrupt the file. Returns nil if no changes
// are pending (no-op).
//
// The map is COPIED under the read lock, then marshaled
// outside the lock. The previous version released the
// read lock before json.MarshalIndent, which then
// iterated the same map Add() was concurrently writing
// to — the race detector caught it on macOS arm64
// (CI run 27732172056). The copy is the canonical
// fix: we read the whole map atomically, then the
// rest of Save operates on a private snapshot.
func (s *Store) Save() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.RLock()
	if !s.dirty {
		s.mu.RUnlock()
		return nil
	}
	// Snapshot the map under the read lock. The
	// copy is a fresh map; mutations to s.m after
	// this point don't affect our snapshot.
	snapshot := make(map[string]Entry, len(s.m))
	for k, v := range s.m {
		snapshot[k] = v
	}
	s.mu.RUnlock()
	f := fileFormat{
		V:     CurrentVersion,
		Entry: snapshot,
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("roster: marshal: %w", err)
	}
	// Atomic write: write to <path>.tmp, then rename.
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, "roster-*.json.tmp")
	if err != nil {
		return fmt.Errorf("roster: create tmp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// On any error path, clean up the tmp.
		_ = os.Remove(tmpName)
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("roster: write tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("roster: close tmp: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("roster: rename: %w", err)
	}
	s.mu.Lock()
	s.dirty = false
	s.mu.Unlock()
	return nil
}

// Close flushes pending changes to disk. Idempotent.
func (s *Store) Close() error {
	return s.Save()
}

// validPeerID is the same lowercase-hex check used
// in internal/alias. We duplicate rather than import
// to keep the leaf-package property (roster is used
// from cmd; alias is too; neither depends on the
// other; importing alias here would create a
// longer-than-needed dependency chain).
func validPeerID(s string) bool {
	if len(s) != peerIDSize {
		return false
	}
	for i := 0; i < peerIDSize; i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
