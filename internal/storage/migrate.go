package storage

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	ic "github.com/weishengsuptp/innerlink/internal/crypto"
)

// migrateLegacyChat is a one-shot migration from the
// v0.5-v1.0 single-file chat.enc layout to the v1.1
// per-peer <chatDir>/<peerID>.enc layout.
//
// Triggered automatically by Open when <saveDir>/chat.enc
// exists. The original file is renamed to chat.enc.migrated-<unix-ts>
// (NOT deleted) so the user can fall back if anything
// looks wrong.
//
// Returns nil if there's no legacy file to migrate (the
// common case after v1.1) or if the migration succeeded.
// Returns an error only if migration was attempted and
// failed; in that case the legacy file is left in place
// and Open logs but does not fail.
func migrateLegacyChat(s *Store, saveDir, chatDir string) error {
	legacyPath := filepath.Join(saveDir, FileName)
	info, err := os.Stat(legacyPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing to migrate
		}
		return fmt.Errorf("stat legacy chat.enc: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("legacy chat.enc is not a regular file")
	}
	if info.Size() == 0 {
		// Empty legacy file — just rename it and move on.
		return renameLegacy(legacyPath)
	}

	// Read + decrypt every record from the legacy file.
	f, err := os.Open(legacyPath)
	if err != nil {
		return fmt.Errorf("open legacy chat.enc: %w", err)
	}
	records, n, err := readAll(f, s.key)
	_ = f.Close()
	if err != nil {
		return fmt.Errorf("decode legacy chat.enc (at byte %d): %w", n, err)
	}
	if len(records) == 0 {
		return renameLegacy(legacyPath)
	}

	// Determine selfPeerID for the pickOther heuristic.
	// We don't have an identity loader here, so use the
	// record metadata:
	//   - Direction="out" → From is self
	//   - Direction="in"  → To is self
	// For each record, count From vs To as "self" to find
	// the most-common hypothesis, then re-derive per-peer.
	// If both From and To appear as self in different records
	// (which is what pkg/node actually writes), the heuristic
	// is exact: for any (rec.From, rec.To) pair, exactly one
	// of them matches the dominant self PeerID.
	selfPeerID := inferSelfFromRecords(records)
	if selfPeerID == "" {
		return fmt.Errorf("could not infer self peer ID from legacy chat.enc")
	}

	// Group by pickOther.
	groups := make(map[string][]*Record)
	order := []string{} // preserve first-seen order for stable filenames
	for _, r := range records {
		other := pickOtherPeerID(r, selfPeerID)
		if other == "" {
			// Skip the malformed record; we still finish
			// migration for the rest.
			continue
		}
		if _, seen := groups[other]; !seen {
			order = append(order, other)
		}
		groups[other] = append(groups[other], r)
	}

	// Write each group to its per-peer file. We use the
	// same per-record encryption as Append (fresh IV per
	// record, length-prefixed frame), so a re-read via
	// the new layout gets the same plaintext records.
	for _, peerID := range order {
		recs := groups[peerID]
		path := filepath.Join(chatDir, PeerFileName(peerID))
		out, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, FileMode)
		if err != nil {
			return fmt.Errorf("open %s: %w", path, err)
		}
		for _, r := range recs {
			plain, err := r.encode()
			if err != nil {
				_ = out.Close()
				return fmt.Errorf("encode record: %w", err)
			}
			iv, err := ic.NewNonce(FrameIVSize)
			if err != nil {
				_ = out.Close()
				return fmt.Errorf("generate IV: %w", err)
			}
			ct, err := ic.SM4EncryptCBC(s.key, iv, plain)
			if err != nil {
				_ = out.Close()
				return fmt.Errorf("encrypt record: %w", err)
			}
			frame := make([]byte, 0, FrameHeaderSize+FrameIVSize+len(ct))
			var lenBuf [FrameHeaderSize]byte
			binary.BigEndian.PutUint32(lenBuf[:], uint32(len(ct)))
			frame = append(frame, lenBuf[:]...)
			frame = append(frame, iv...)
			frame = append(frame, ct...)
			if _, err := out.Write(frame); err != nil {
				_ = out.Close()
				return fmt.Errorf("write frame to %s: %w", path, err)
			}
		}
		if err := out.Sync(); err != nil {
			_ = out.Close()
			return fmt.Errorf("sync %s: %w", path, err)
		}
		if err := out.Close(); err != nil {
			return fmt.Errorf("close %s: %w", path, err)
		}
	}

	// Rename the legacy file as a backup. We do NOT delete
	// it — the user can sweep manually after confirming the
	// new layout works for them.
	if err := renameLegacy(legacyPath); err != nil {
		return fmt.Errorf("rename legacy: %w", err)
	}
	return nil
}

// renameLegacy moves the legacy chat.enc aside to
// chat.enc.migrated-<unix-ts>. Idempotent (if the target
// name already exists — should not happen in normal use —
// we suffix with a counter to avoid overwriting).
func renameLegacy(legacyPath string) error {
	dir := filepath.Dir(legacyPath)
	base := "chat.enc.migrated"
	target := filepath.Join(dir, fmt.Sprintf("%s-%d", base, time.Now().Unix()))
	for i := 1; ; i++ {
		if _, err := os.Stat(target); err != nil {
			break
		}
		target = filepath.Join(dir, fmt.Sprintf("%s-%d-%d", base, time.Now().Unix(), i))
	}
	if err := os.Rename(legacyPath, target); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", legacyPath, target, err)
	}
	return nil
}

// inferSelfFromRecords guesses the device's own PeerID
// from a slice of records. We assume:
//   - Direction="out" → From is self
//   - Direction="in"  → To is self
// and pick the PeerID that appears in those slots most
// often. Tie-broken by lexicographic order for stability.
//
// Returns "" if records is empty or the heuristic gives
// no consistent answer.
func inferSelfFromRecords(records []*Record) string {
	if len(records) == 0 {
		return ""
	}
	counts := make(map[string]int)
	for _, r := range records {
		switch r.Direction {
		case "out":
			if r.From != "" {
				counts[r.From]++
			}
		case "in":
			if r.To != "" {
				counts[r.To]++
			}
		}
	}
	if len(counts) == 0 {
		return ""
	}
	// Sort by count desc, then peerID asc for stability.
	type kv struct {
		k string
		v int
	}
	var list []kv
	for k, v := range counts {
		list = append(list, kv{k, v})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].v != list[j].v {
			return list[i].v > list[j].v
		}
		return list[i].k < list[j].k
	})
	return list[0].k
}

// keep io import referenced.
var _ = io.EOF