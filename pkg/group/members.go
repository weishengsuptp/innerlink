package group

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	Members   []Member  `json:"members"`
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

// Save writes m to <dataDir>/groups/<renderedID>/members.json
// atomically (write to .tmp + rename) so a crash mid-write never
// leaves a half-baked file. Creates the directory if missing.
//
// 0600 / 0700 perms: members.json leaks peerIDs (not secret, but
// not public either — knowing someone's peerID enables direct
// messaging without their consent). Group directory is owner-only.
func (m *Members) Save(dataDir string, rawGroupID []byte) error {
	if len(rawGroupID) != RawGroupIDSize {
		return fmt.Errorf("group: Save: bad GroupID size %d, want %d",
			len(rawGroupID), RawGroupIDSize)
	}
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
