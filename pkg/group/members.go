package group

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

// Member is one row in a group's roster. The IsCreator flag
// distinguishes the founding peer (always present, never removed
// except via "解散群" which is out of v1.1 scope) from invitees.
type Member struct {
	PeerID    string    `json:"peer_id"`
	Alias     string    `json:"alias,omitempty"`
	JoinedAt  time.Time `json:"joined_at"`
	IsCreator bool      `json:"is_creator,omitempty"`
}

// Members is the on-disk roster for one group. Persisted as JSON
// in <dataDir>/groups/<renderedGroupID>/members.json.
//
// Why a separate file per group (rather than a global groups.json
// or a single chat.enc-style stream):
//
//   - Atomic per-group writes — a roster update for group X never
//     touches the on-disk file of group Y.
//   - Cheap per-group directory listing — `ls groups/` shows every
//     group this peer knows about, no DB needed.
//   - Mirrors the per-peer chat file layout that landed in v1.1
//     phase 1 (storage.Store per-peer chat/<peerID>.enc) — same
//     shape, easy to reason about.
type Members struct {
	GroupID   string    `json:"group_id"`   // rendered "g_<hex>"
	GroupName string    `json:"group_name"`
	Creator   string    `json:"creator"`    // creator's peerID (also in Members[0])
	CreatedAt time.Time `json:"created_at"`
	// Remark is the group's user-editable note / announcement
	// (WeChat-style "群公告"-ish). Optional; "" means unset.
	// v1.1.1 (2026-06-29). Mirrored across all members via
	// TypeGroupMetaUpdate; receivers update their local
	// members.json so the sidebar + settings panel show
	// the same value across peers. Old files without this field
	// unmarshal to "" (json default).
	Remark   string   `json:"remark,omitempty"`
	Members  []Member `json:"members"`
	// LastModified — v1.1.6 (2026-07-05) — touch timestamp
	// of this local m.Members. Updated on every saveLocked.
	// Carried in rosterPayload so receivers can decide
	// "did this remote view post-date my local view?" and
	// refuse to overwrite a fresher local with a stale
	// inbound.
	//
	// Zero value: this file has never been touched by a
	// v1.1.6+ binary (created pre-LM and never re-saved
	// since, or saved by a backlevel binary). The
	// ApplyRosterUpdate accept/refuse rule treats zero
	// specially:
	//   local.LastModified > 0 AND inbound.LastModified <= local:
	//     refuse (inbound is stale relative to my local).
	//   otherwise: accept (covers pre-LM era, fresh info,
	//     and inbound-only-from-fresher-binary paths).
	LastModified time.Time `json:"last_modified,omitempty"`
}

// membersFileName is exported as a constant so tests and storage
// code can refer to the on-disk path without string-typing it.
const membersFileName = "members.json"

// groupDir returns the absolute path to a group's directory
// under dataDir. dataDir is expected to be the user's
// <data-dir> root (e.g. AppDataDir() on Windows, ~/Library/... on
// macOS). Caller is responsible for ensuring dataDir exists.
func groupDir(dataDir string, rawGroupID []byte) string {
	return filepath.Join(dataDir, "groups", RenderGroupID(rawGroupID))
}

// membersPath is the full path to a group's members.json.
func membersPath(dataDir string, rawGroupID []byte) string {
	return filepath.Join(groupDir(dataDir, rawGroupID), membersFileName)
}

// LoadMembers reads <dataDir>/groups/<renderedID>/members.json.
// Returns os.ErrNotExist if the group has never been initialized
// on this peer (caller decides whether that's an error or a
// "haven't joined yet" signal).
func LoadMembers(dataDir string, rawGroupID []byte) (*Members, error) {
	if len(rawGroupID) != RawGroupIDSize {
		return nil, fmt.Errorf("group: LoadMembers: bad GroupID size %d, want %d",
			len(rawGroupID), RawGroupIDSize)
	}
	// Acquire the per-group lock so we never read while
	// a concurrent Save is mid-rename. Without this, a
	// reader can see a partial state (or a half-renamed
	// file) on Windows.
	mu := lockFor(rawGroupID)
	mu.Lock()
	defer mu.Unlock()
	b, err := os.ReadFile(membersPath(dataDir, rawGroupID))
	if err != nil {
		return nil, err
	}
	var m Members
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("group: parse members.json: %w", err)
	}
	return &m, nil
}

// UpdateMembers runs fn under the per-group lock with the
// current Members loaded. fn may mutate the in-memory
// Members in any way; the result is auto-saved before
// the lock is released. Returns the post-mutation m
// (== the in-memory Members pointer that fn was called
// with, after Save) so callers can broadcast /
// publish from a consistent state.
//
// Use this for read-modify-write operations (e.g.
// "add a member if not already present") that must be
// atomic against concurrent writers. Pure reads should
// use LoadMembers; pure writes that don't need to look
// at the current state can use Save directly (after
// LoadMembers).
//
// On a fresh group (members.json doesn't exist), fn is
// called with an empty Members struct + nil error, and
// the result is written (if fn didn't return an error).
//
// Why this exists: a previous version of pkg/node used
// the pattern "LoadMembers → mutate → Save" which is
// NOT atomic against another goroutine doing the same
// thing — the in-memory mutation is outside the lock,
// so two concurrent adds can both read the same base
// state and overwrite each other on Save. This helper
// closes the gap. Caught by S5 in the integration test
// harness (2026-07-03).
func UpdateMembers(dataDir string, rawGroupID []byte, fn func(*Members) error) (*Members, error) {
	if len(rawGroupID) != RawGroupIDSize {
		return nil, fmt.Errorf("group: UpdateMembers: bad GroupID size %d, want %d",
			len(rawGroupID), RawGroupIDSize)
	}
	mu := lockFor(rawGroupID)
	mu.Lock()
	defer mu.Unlock()

	b, err := os.ReadFile(membersPath(dataDir, rawGroupID))
	m := &Members{}
	if err == nil {
		if err := json.Unmarshal(b, m); err != nil {
			return nil, fmt.Errorf("group: parse members.json: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	if err := fn(m); err != nil {
		return nil, err
	}

	if err := m.saveLocked(dataDir, rawGroupID); err != nil {
		return nil, err
	}
	return m, nil
}

// saveLocked writes m to members.json atomically. MUST be
// called with the per-group lock held (via UpdateMembers
// or by direct coordination). The Save() public method
// acquires the lock itself for callers that don't already
// hold it; this version is for the locked code path.
func (m *Members) saveLocked(dataDir string, rawGroupID []byte) error {
	// v1.1.6 — stamp every save with the local wall-clock
	// time so receivers can refuse strictly-older inbound
	// roster views. We update LastModified BEFORE marshalling
	// so the value lands in this exact file write, even on
	// racing writers (each acquires the per-group saveLock).
	m.LastModified = time.Now().UTC()
	dir := groupDir(dataDir, rawGroupID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("group: mkdir %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("group: marshal members.json: %w", err)
	}
	final := filepath.Join(dir, membersFileName)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("group: write tmp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("group: rename %s -> %s: %w", tmp, final, err)
	}
	return nil
}

// Save writes m to <dataDir>/groups/<renderedID>/members.json
// atomically (write to .tmp + rename) so a crash mid-write never
// leaves a half-baked file. Creates the directory if missing.
//
// 0600 / 0700 perms: members.json leaks peerIDs (not secret, but
// not public either — knowing someone's peerID enables direct
// messaging without their consent). Group directory is owner-only.
// saveLocks serializes Save / LoadMembers for the same
// rawGroupID across the process. Multiple concurrent
// writers (e.g. two CreatorOnAccept envelopes arriving
// simultaneously) would otherwise race on the file-system
// rename and one would fail with "Access is denied" on
// Windows. Per-group locking keeps the contention local
// to one group and lets unrelated groups proceed in
// parallel.
//
// The map is keyed by the rendered GroupID string — fine
// for process-local locking; not a substitute for the
// in-memory mutex that pkg/node uses elsewhere.
var saveLocks sync.Map // map[string]*sync.Mutex

func lockFor(rawGroupID []byte) *sync.Mutex {
	key := string(rawGroupID)
	v, _ := saveLocks.LoadOrStore(key, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func (m *Members) Save(dataDir string, rawGroupID []byte) error {
	if len(rawGroupID) != RawGroupIDSize {
		return fmt.Errorf("group: Save: bad GroupID size %d, want %d",
			len(rawGroupID), RawGroupIDSize)
	}
	// Serialize Save+LoadMembers for the same group. The
	// lock is process-wide (any caller touching this
	// group's members.json — Node, harness, recovery
	// path — will queue behind us).
	mu := lockFor(rawGroupID)
	mu.Lock()
	defer mu.Unlock()

	// v1.1.6 — stamp save timestamp before marshal so this
	// exact write carries LastModified; receivers can then
	// order inbound roster views by freshness.
	m.LastModified = time.Now().UTC()
	dir := groupDir(dataDir, rawGroupID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("group: mkdir %s: %w", dir, err)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("group: marshal members.json: %w", err)
	}
	final := filepath.Join(dir, membersFileName)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("group: write tmp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		// Best-effort cleanup of the .tmp on rename failure.
		_ = os.Remove(tmp)
		return fmt.Errorf("group: rename %s -> %s: %w", tmp, final, err)
	}
	return nil
}

// AddMember appends a member and re-saves. Returns an error if
// the peer is already in the roster (caller should treat that
// as "already joined" rather than retrying).
//
// Caller is responsible for generating JoinedAt appropriately
// (use time.Now().UTC() for consistency across peers).
func (m *Members) AddMember(mem Member) error {
	if mem.PeerID == "" {
		return errors.New("group: AddMember: empty peerID")
	}
	if m.Contains(mem.PeerID) {
		return fmt.Errorf("group: peer %s already in %s", shortID(mem.PeerID), m.GroupID)
	}
	m.Members = append(m.Members, mem)
	// Keep roster sorted by JoinedAt so the on-disk file is
	// stable across writes from the same peer. Across peers
	// the order may differ (each peer's local clock), but the
	// bytes-on-disk determinism is what matters for diff-ability.
	sort.SliceStable(m.Members, func(i, j int) bool {
		return m.Members[i].JoinedAt.Before(m.Members[j].JoinedAt)
	})
	return nil
}

// RemoveMember drops a peer from the roster. The Creator cannot
// be removed (callers should check before calling if they want
// a friendly error). Returns false if peerID was not present.
func (m *Members) RemoveMember(peerID string) bool {
	for i, mem := range m.Members {
		if mem.PeerID != peerID {
			continue
		}
		if mem.IsCreator {
			// Defensive: callers should special-case Creator
			// removal as "dissolve group", which is a separate
			// flow not yet implemented in v1.1.
			return false
		}
		m.Members = append(m.Members[:i], m.Members[i+1:]...)
		return true
	}
	return false
}

// Contains reports whether peerID is in the roster.
func (m *Members) Contains(peerID string) bool {
	for _, mem := range m.Members {
		if mem.PeerID == peerID {
			return true
		}
	}
	return false
}

// shortID returns the first 8 chars of a peerID for friendly logs.
// Helper kept here to avoid pulling in pkg/node just for logs.
func shortID(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}
