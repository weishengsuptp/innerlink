package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// ReadAll loads every record from every per-peer file
// under <saveDir>/chat/ and returns them in append order
// (oldest first, across all peers). The returned slice
// is freshly allocated.
//
// Per-peer files are read sequentially (one at a time);
// records from different peers are interleaved only by
// timestamp on the record itself (we sort at the end).
// This is intentional: ReadAll holds the store's readAllMu
// to prevent a concurrent Append from interleaving, but
// it's a startup-once path, not a hot path.
//
// If chat/ does not exist or is empty (first launch, or
// after the user deleted all chats) ReadAll returns
// (nil, nil) — there is no history yet, not an error.
func (s *Store) ReadAll() ([]*Record, error) {
	s.readAllMu.Lock()
	defer s.readAllMu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("storage: read chat dir: %w", err)
	}
	var all []*Record
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !isPeerFileName(name) {
			continue
		}
		path := filepath.Join(s.dir, name)
		f, err := os.Open(path)
		if err != nil {
			// Per-peer file unreadable — skip it. We don't
			// fail the whole ReadAll because one corrupted
			// peer shouldn't make the rest of the history
			// invisible. The chat.enc-era error semantics
			// (stop on first corruption, return what we
			// have + ErrCorrupt) is too aggressive for the
			// per-peer layout — a single bad peer would
			// hide every other peer's history.
			continue
		}
		recs, _, rerr := readAll(f, s.key)
		_ = f.Close()
		if rerr != nil && rerr != ErrCorrupt {
			// Real error (not just truncated/corrupt):
			// skip and keep going.
			continue
		}
		all = append(all, recs...)
	}
	sortByTimestamp(all)
	return all, nil
}

// isPeerFileName returns true if name matches the canonical
// per-peer file pattern: 32 lowercase hex chars + ".enc".
// Any other file in chat/ is ignored (so the user can
// drop a README.txt or chat.bak in there without confusing
// us).
func isPeerFileName(name string) bool {
	if len(name) != 32+len(PeerFileExt) {
		return false
	}
	if name[len(name)-len(PeerFileExt):] != PeerFileExt {
		return false
	}
	stem := name[:len(name)-len(PeerFileExt)]
	return isValidPeerID(stem)
}

// sortByTimestamp sorts records in place by their Timestamp
// field, oldest first. Ties preserve the input order
// (stable sort).
func sortByTimestamp(rs []*Record) {
	sort.SliceStable(rs, func(i, j int) bool {
		return rs[i].Timestamp.Before(rs[j].Timestamp)
	})
}