package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// groupFile mirrors peerFile but lives under
// <dataDir>/groups/<groupID>/chat.enc (one per group).
// Records appended here are full storage.Record values
// (same shape as peer chat) — only the directory layout
// differs. v1.1 groups piggy-back on the existing per-peer
// frame format ([4B len][16B IV][N B SM4-CBC ciphertext])
// and the same device.key-derived encryption key.
//
// Why one file per group (not one big groups.enc):
//   - Same atomic-write / append-only discipline as per-peer
//   - Per-group delete = delete the directory, no other
//     group's data is touched
//   - HistoryGroup() reads only one file (cheap)
type groupFile struct {
	mu       sync.Mutex
	path     string
	key      []byte
}

const (
	// GroupDirName is the directory under dataDir that holds
	// per-group subdirectories. Each group gets its own
	// <groupID>/chat.enc plus a members.json + sender-keys/
	// (those files live in pkg/group, this package only
	// owns the chat persistence).
	GroupDirName = "groups"
	// GroupChatFileName is the encrypted chat log for one group.
	GroupChatFileName = "chat.enc"
)

// groupFilePath returns <dataDir>/groups/<groupID>/chat.enc.
// groupID is the rendered form ("g_<hex>") — we keep the
// directory name human-readable so `ls data/groups/` shows
// what groups exist on this peer.
func groupFilePath(dataDir, renderedGroupID string) string {
	return filepath.Join(dataDir, GroupDirName, renderedGroupID, GroupChatFileName)
}

// AppendGroup appends rec to <dataDir>/groups/<renderedGroupID>/chat.enc.
// The record's GroupID field MUST equal renderedGroupID (caller
// is responsible for setting it; we assert and refuse otherwise).
//
// AppendGroup uses the same SM4-CBC frame format as the per-peer
// chat.enc, and the same device.key-derived encryption key — so a
// single chat reader can decrypt both.
//
// Locking: serialized per-group via groupFile.mu. Different
// groups may Append concurrently without blocking each other.
func (s *Store) AppendGroup(renderedGroupID string, rec *Record) error {
	if rec == nil {
		return errors.New("storage: AppendGroup: nil record")
	}
	if rec.GroupID == "" {
		return errors.New("storage: AppendGroup: record has empty GroupID")
	}
	if rec.GroupID != renderedGroupID {
		return fmt.Errorf("storage: AppendGroup: record.GroupID=%q != renderedGroupID=%q",
			rec.GroupID, renderedGroupID)
	}
	// For groups the From/To fields are unused (peer-to-peer
	// addressing doesn't apply). We use From = sender peerID
	// (matches the in-message from-line in the GUI), and
	// leave To = "" (no specific recipient).
	if rec.From == "" {
		return errors.New("storage: AppendGroup: record.From is empty (sender peerID required)")
	}
	s.groupMu.Lock()
	gf, err := s.openGroupFile(renderedGroupID)
	s.groupMu.Unlock()
	if err != nil {
		return err
	}
	gf.mu.Lock()
	defer gf.mu.Unlock()
	return writeFrame(gf.path, gf.key, rec)
}

// HistoryGroup reads every record from
// <dataDir>/groups/<renderedGroupID>/chat.enc, sorted
// oldest-first. Returns an empty slice if the file doesn't
// exist (group has no history yet — normal for a freshly-
// created group).
func (s *Store) HistoryGroup(renderedGroupID string) ([]*Record, error) {
	path := groupFilePath(s.SaveDir(), renderedGroupID)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // fresh group, no history
		}
		return nil, err
	}
	recs, err := readAllBytes(b, s.key)
	if err != nil {
		return nil, fmt.Errorf("storage: decode group chat.enc: %w", err)
	}
	sort.SliceStable(recs, func(i, j int) bool {
		return recs[i].Timestamp.Before(recs[j].Timestamp)
	})
	return recs, nil
}

// DeleteGroup deletes the entire group directory (chat.enc +
// members.json + sender-keys/). Used when leaving a group or
// when a creator dissolves one. Returns nil if the directory
// didn't exist (idempotent).
func (s *Store) DeleteGroup(renderedGroupID string) error {
	dir := filepath.Join(s.SaveDir(), GroupDirName, renderedGroupID)
	err := os.RemoveAll(dir)
	if err != nil {
		return fmt.Errorf("storage: delete group %s: %w", renderedGroupID, err)
	}
	return nil
}

// ListGroups returns the rendered GroupIDs of every group
// with a chat.enc on disk. Sorted for stable display.
func (s *Store) ListGroups() ([]string, error) {
	base := filepath.Join(s.SaveDir(), GroupDirName)
	entries, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // no groups yet
		}
		return nil, fmt.Errorf("storage: list groups: %w", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Verify the chat.enc actually exists — empty
		// directories left behind from interrupted
		// setup shouldn't show up.
		chatPath := filepath.Join(base, e.Name(), GroupChatFileName)
		if _, err := os.Stat(chatPath); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// openGroupFile opens (creating if missing) the per-group
// chat.enc and returns a cached groupFile handle. Holds
// s.groupMu only for the open/insert into the map, then
// releases — the groupFile's own mu handles per-record
// append serialization.
func (s *Store) openGroupFile(renderedGroupID string) (*groupFile, error) {
	if s.groupFiles == nil {
		s.groupFiles = make(map[string]*groupFile)
	}
	if gf, ok := s.groupFiles[renderedGroupID]; ok {
		return gf, nil
	}
	dir := filepath.Join(s.SaveDir(), GroupDirName, renderedGroupID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("storage: mkdir %s: %w", dir, err)
	}
	gf := &groupFile{
		path: groupFilePath(s.SaveDir(), renderedGroupID),
		key:  s.key,
	}
	s.groupFiles[renderedGroupID] = gf
	return gf, nil
}
