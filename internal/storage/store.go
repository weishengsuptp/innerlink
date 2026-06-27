package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	ic "github.com/weishengsuptp/innerlink/internal/crypto"
)

// ChatDirName is the sub-directory under saveDir that holds
// per-peer chat files. v1.1 (2026-06-27) split the legacy
// single chat.enc into one <peerID>.enc per peer; the
// directory layout looks like:
//
//	<saveDir>/
//	├── chat/
//	│   ├── 268fb7ee158d35160ef9ef76e0977f2a.enc
//	│   ├── abc123...def...                       (32-char hex)
//	│   └── ...
//	├── chat.enc.migrated-1719465231   ← legacy backup (NOT deleted)
//
// Per-peer files are named by the 32-char lowercase hex
// PeerID of the OTHER party (i.e. the peer the chat is
// with), so a single conversation is local to one file on
// BOTH devices (each device stores the conversation under
// the other's peer ID).
//
// Why this exists: the user wants "delete all chat with
// peer X" to be a real action. The legacy single-file
// layout made that either-all-or-nothing; v1.1 makes it
// per-peer.
//
// See V1.1-PLAN.md (process doc, not in repo) §3 for the
// design rationale.
const ChatDirName = "chat"

// PeerFileExt is the per-peer chat file extension.
const PeerFileExt = ".enc"

// PeerFileName returns the canonical file name for the chat
// log with peer (32-char hex PeerID). panics if peerID is
// not 32 lowercase hex chars (caller bug — we don't want
// to silently produce a malformed filename).
func PeerFileName(peerID string) string {
	if !isValidPeerID(peerID) {
		// Don't recover from a bad input — every caller
		// either has the PeerID from a parsed envelope
		// (always 32 hex) or from resolvePeerRef (also
		// returns 32 hex). A bad value here is a bug.
		panic("storage: peerID must be 32 lowercase hex chars, got " + peerID)
	}
	return peerID + PeerFileExt
}

func isValidPeerID(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// Store is the per-device encrypted chat log, persisted as
// one file per peer under <saveDir>/chat/. The legacy
// single-file layout (<saveDir>/chat.enc) is auto-migrated
// on first Open.
//
// Store is safe for concurrent use: many goroutines may
// Append simultaneously (each routes to its own peer file
// so writes don't actually interleave on disk; the mutex
// just guards the per-peer file-handle map so we don't open
// the same file twice from concurrent Appends). ReadAll
// holds the mutex while it walks the directory so it sees
// a consistent snapshot of "what peer files existed at
// this moment".
//
// Lifecycle:
//
//	st, err := storage.Open(saveDir, id.PrivateKeyD())
//	if err != nil { ... }
//	st.SetSelfPeerID(id.PeerIDHex())  // required before Append
//	// ... use st.Append() / st.History() / st.DeleteAllForPeer() ...
//	if err := st.Close(); err != nil { ... }
type Store struct {
	dir        string             // <saveDir>/chat/ (auto-created by Open)
	key        []byte             // 16-byte SM4 key (KDF from deviceD)
	selfPeerID string             // set via SetSelfPeerID before Append
	filesMu    sync.Mutex         // guards files map + unsynced counter
	files      map[string]*peerFile // peerID (32 hex) -> open handle + unsynced count
	readAllMu  sync.Mutex         // serializes ReadAll (read-then-close each file)
	closed     bool               // guarded by filesMu
}

// peerFile is one open per-peer chat file. Each file has
// its own unsynced counter so a hot peer doesn't dominate
// fsyncs for a quiet peer.
type peerFile struct {
	f        *os.File
	unsynced int
}

// Open prepares <saveDir>/chat/, derives the SM4 key from
// deviceD, and auto-migrates any legacy chat.enc into the
// new per-peer layout. The returned Store is ready for
// SetSelfPeerID + Append + ReadAll + DeleteAllForPeer.
//
// If chat.enc exists at <saveDir>/chat.enc (the v0.5–v1.0
// single-file layout), Open runs a one-shot migration:
//   1. Read every record from chat.enc.
//   2. Group by pickOther(rec.From, rec.To, selfPeerID).
//      (selfPeerID is read from <saveDir>/device.key's
//      canonical self — but we don't have a device loader
//      here, so we use a heuristic: assume self is the
//      "From" for Direction="out" and "To" for Direction="in".
//      This is exact because pkg/node always writes it
//      that way.)
//   3. Write each group to <saveDir>/chat/<peerID>.enc.
//   4. Rename the old file to <saveDir>/chat.enc.migrated-<unix-ts>
//      (NOT deleted — user can sweep manually).
//   5. If anything goes wrong, the original chat.enc is
//      left untouched and the migration can be retried on
//      next Open.
//
// deviceD is the 32-byte big-endian SM2 private scalar.
// Same as before.
func Open(saveDir string, deviceD []byte) (*Store, error) {
	if len(deviceD) != 32 {
		return nil, fmt.Errorf("storage: device key must be 32 bytes, got %d", len(deviceD))
	}
	if err := os.MkdirAll(saveDir, DirMode); err != nil {
		return nil, fmt.Errorf("storage: mkdir save dir: %w", err)
	}
	key, err := ic.KDF(deviceD, []byte(keyDerivationInfo), KeySize)
	if err != nil {
		return nil, fmt.Errorf("storage: derive SM4 key: %w", err)
	}
	chatDir := filepath.Join(saveDir, ChatDirName)
	if err := os.MkdirAll(chatDir, DirMode); err != nil {
		return nil, fmt.Errorf("storage: mkdir chat dir: %w", err)
	}
	s := &Store{
		dir:   chatDir,
		key:   key,
		files: make(map[string]*peerFile),
	}
	// Auto-migrate legacy chat.enc if present. Failure here
	// is logged-but-not-fatal: the user can still use the
	// new layout with whatever records they have post-migration.
	if err := migrateLegacyChat(s, saveDir, chatDir); err != nil {
		// Don't fail Open — the per-peer store works fine
		// even if migration left the old file in place.
		// Log via fmt.Fprintln(os.Stderr) so it shows up in
		// innerlink.log without us needing to plumb logx
		// into this package.
		fmt.Fprintf(os.Stderr, "[WARN ] storage: legacy chat.enc migration: %v\n", err)
	}
	return s, nil
}

// SaveDir returns the chat directory the Store was opened
// against (<saveDir>/chat/). Used by callers that need to
// show the user where the per-peer files live.
func (s *Store) SaveDir() string {
	return s.dir
}

// SetSelfPeerID configures which side of the from/to pair
// is "us". Must be called before Append; Append without
// SetSelfPeerID returns an error (we'd otherwise have to
// guess, and a wrong guess puts the conversation in the
// wrong per-peer file — invisible corruption).
//
// SetSelfPeerID is idempotent; calling it twice replaces
// the value. It is intentionally not thread-safe with
// concurrent Append — callers (pkg/node) configure self
// once at startup before any Append can happen.
func (s *Store) SetSelfPeerID(peerID string) {
	if !isValidPeerID(peerID) {
		// same panic rationale as PeerFileName
		panic("storage: SetSelfPeerID requires 32 lowercase hex chars, got " + peerID)
	}
	s.selfPeerID = peerID
}

// Append encrypts r and writes it to the per-peer file
// for the OTHER side of the conversation (the peer on the
// other side of from/to from selfPeerID).
//
// Append holds the store's filesMu only long enough to
// look up + open (if needed) the peer file, then drops it
// and writes under the per-file handle. This means
// concurrent Appends to DIFFERENT peers run in parallel
// (real disk parallelism); concurrent Appends to the SAME
// peer serialize on the file's per-handle write (which
// is what os.File.Write requires anyway).
//
// Each peer's file gets its own fsync cadence (one sync
// per recordsPerSync writes to that peer), so a chatty
// peer doesn't drag fsyncs for a quiet one.
func (s *Store) Append(r *Record) error {
	if r == nil {
		return errors.New("storage: nil record")
	}
	if r.Version == 0 {
		r.Version = CurrentVersion
	}
	if s.selfPeerID == "" {
		return errors.New("storage: Append before SetSelfPeerID")
	}
	other := pickOtherPeerID(r, s.selfPeerID)
	if other == "" {
		// selfPeerID matches neither from nor to — the
		// record is malformed (or selfPeerID is wrong).
		// Refuse rather than write to a phantom file.
		return fmt.Errorf("storage: record has no peer on the other side (from=%s to=%s self=%s)",
			r.From, r.To, s.selfPeerID)
	}

	plain, err := r.encode()
	if err != nil {
		return fmt.Errorf("storage: encode record: %w", err)
	}
	iv, err := ic.NewNonce(FrameIVSize)
	if err != nil {
		return fmt.Errorf("storage: generate IV: %w", err)
	}
	ct, err := ic.SM4EncryptCBC(s.key, iv, plain)
	if err != nil {
		return fmt.Errorf("storage: encrypt record: %w", err)
	}
	frame := make([]byte, 0, FrameHeaderSize+FrameIVSize+len(ct))
	var lenBuf [FrameHeaderSize]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(ct)))
	frame = append(frame, lenBuf[:]...)
	frame = append(frame, iv...)
	frame = append(frame, ct...)

	// Open (or create) the per-peer file. The map lookup +
	// create + os.OpenFile is done under filesMu so two
	// concurrent first-time Appends for the same peer don't
	// both open the file. After we have *peerFile we drop
	// the mutex and write under the per-handle mutex-free
	// path (os.File.Write is goroutine-safe).
	s.filesMu.Lock()
	if s.closed {
		s.filesMu.Unlock()
		return ErrClosed
	}
	pf, ok := s.files[other]
	if !ok {
		path := filepath.Join(s.dir, PeerFileName(other))
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, FileMode)
		if err != nil {
			s.filesMu.Unlock()
			return fmt.Errorf("storage: open peer file %s: %w", other, err)
		}
		pf = &peerFile{f: f}
		s.files[other] = pf
	}
	s.filesMu.Unlock()

	if _, err := pf.f.Write(frame); err != nil {
		return fmt.Errorf("storage: write peer file %s: %w", other, err)
	}
	pf.unsynced++
	if pf.unsynced >= recordsPerSync {
		// Sync without holding filesMu — fsync is a syscall
		// that can take a few ms on Windows; not blocking
		// other peers' Appends.
		if err := pf.f.Sync(); err != nil {
			return fmt.Errorf("storage: sync peer file %s: %w", other, err)
		}
		pf.unsynced = 0
	}
	return nil
}

// pickOtherPeerID returns the peer ID on the other side of
// a record relative to self. self must appear in exactly
// one of from/to; if neither matches, returns "".
func pickOtherPeerID(r *Record, self string) string {
	switch {
	case r.From == self && r.To == self:
		// self-chat (loopback) — file under self.
		return self
	case r.From == self:
		return r.To
	case r.To == self:
		return r.From
	default:
		return ""
	}
}

// DeleteAllForPeer removes the per-peer chat file for
// peerID and forgets the open handle. Returns nil if the
// file doesn't exist (idempotent — "delete what's there").
//
// The peerID must be a 32-char lowercase hex string; any
// other value is rejected (we don't want a stray ".." or
// shell metacharacter to walk the directory tree). Pass
// pkg/node's resolved hex form; never a user-supplied
// alias name.
//
// This does NOT modify the in-memory history slice held
// by pkg/node — pkg/node.DeleteHistory does that after
// calling this. Caller responsibility.
func (s *Store) DeleteAllForPeer(peerID string) error {
	if !isValidPeerID(peerID) {
		return fmt.Errorf("storage: DeleteAllForPeer requires 32 lowercase hex chars, got %q", peerID)
	}
	s.filesMu.Lock()
	if pf, ok := s.files[peerID]; ok {
		// Close + drop the handle so a subsequent Append
		// (somehow racing) gets a fresh file from disk
		// (which will be empty → recreates the file).
		_ = pf.f.Close()
		delete(s.files, peerID)
	}
	s.filesMu.Unlock()
	path := filepath.Join(s.dir, PeerFileName(peerID))
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("storage: delete peer file %s: %w", peerID, err)
	}
	return nil
}

// ListPeers returns the sorted list of peer IDs (32-char
// hex) that have at least one record on disk. Used by
// the GUI's history-availability check and by tests.
//
// Note: does NOT include the self-peer ID unless there is
// a "self-chat" record (loopback test case). For typical
// LAN use this just lists the people you've chatted with.
func (s *Store) ListPeers() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("storage: read chat dir: %w", err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, PeerFileExt) {
			continue
		}
		// Strip .enc and validate. We don't want to list
		// files that don't match the canonical naming
		// (e.g. user dropped chat.bak.enc into the dir).
		stem := strings.TrimSuffix(name, PeerFileExt)
		if isValidPeerID(stem) {
			out = append(out, stem)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Close flushes and closes every open per-peer file.
// Idempotent.
func (s *Store) Close() error {
	s.filesMu.Lock()
	if s.closed {
		s.filesMu.Unlock()
		return nil
	}
	s.closed = true
	files := s.files
	s.files = nil
	s.filesMu.Unlock()
	var firstErr error
	for _, pf := range files {
		if err := pf.f.Sync(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("storage: sync: %w", err)
		}
		if err := pf.f.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("storage: close: %w", err)
		}
	}
	return firstErr
}

// readAll is unchanged from v1.0 — it operates on a single
// io.Reader and is reused by ReadAll (which opens each
// per-peer file in turn and concatenates the decoded
// records).
func readAll(r io.Reader, key []byte) (records []*Record, n int, err error) {
	br := newByteReader(r)
	for {
		var lenBuf [FrameHeaderSize]byte
		readN, rerr := io.ReadFull(br, lenBuf[:])
		if readN == 0 && rerr == io.EOF {
			return records, br.offset, nil
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			if len(records) == 0 {
				return records, br.offset, nil
			}
			return records, br.offset - readN, ErrCorrupt
		}
		if rerr != nil {
			return records, br.offset, fmt.Errorf("storage: read header: %w", rerr)
		}
		frameLen := binary.BigEndian.Uint32(lenBuf[:])
		const maxFrame = 1 << 20 // 1 MiB
		if frameLen == 0 || frameLen > maxFrame {
			return records, br.offset, ErrCorrupt
		}
		frame := make([]byte, FrameIVSize+int(frameLen))
		readN, rerr = io.ReadFull(br, frame)
		if rerr != nil {
			return records, br.offset, fmt.Errorf("storage: read frame body: %w", rerr)
		}
		iv := frame[:FrameIVSize]
		ct := frame[FrameIVSize:]
		plain, derr := ic.SM4DecryptCBC(key, iv, ct)
		if derr != nil {
			return records, br.offset, ErrCorrupt
		}
		rec, derr := decodeRecord(plain)
		if derr != nil {
			return records, br.offset, fmt.Errorf("storage: decode record: %w", derr)
		}
		records = append(records, rec)
	}
}

type byteReader struct {
	r      io.Reader
	offset int
}

func newByteReader(r io.Reader) *byteReader { return &byteReader{r: r} }

func (b *byteReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.offset += n
	return n, err
}

// keep io import referenced for the helpers.
var _ = io.EOF