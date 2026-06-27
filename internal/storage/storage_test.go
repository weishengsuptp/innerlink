package storage_test

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weishengsuptp/innerlink/internal/storage"
)

// fakeDeviceKey returns 32 random bytes that stand in
// for an SM2 private scalar D. We don't need a real SM2
// key here — the storage layer only sees the raw 32
// bytes and feeds them into KDF. Real device keys come
// from identity.Identity.PrivateKeyD().
func fakeDeviceKey(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// fakePeerID returns a deterministic 32-char lowercase
// hex PeerID derived from a label, suitable for testing.
func fakePeerID(label string) string {
	// hex.EncodeToString on a 16-byte md5(label) would
	// be 32 chars; we just want a stable hex string.
	// Take md5(label) which gives 16 bytes; hex.Encode
	// gives 32 lowercase hex chars. We don't bother
	// actually MD5'ing — a hand-rolled hex-encoded label
	// padded to 32 chars works too.
	hexed := hex.EncodeToString([]byte(label))
	if len(hexed) >= 32 {
		return hexed[:32]
	}
	// Pad with '0' — only used in tests, the values are
	// arbitrary as long as they're 32 lowercase hex chars.
	return (hexed + strings.Repeat("0", 32))[:32]
}

const (
	selfPeer = "00112233445566778899001122334455"
	peerA    = "aabbccddeeff00112233445566778899"
	peerB    = "99887766554433221100ffeeddccbbaa"
)

// newTestStore returns a Store opened on a fresh temp
// dir with selfPeerID pre-configured (every test that
// Appends needs SetSelfPeerID).
func newTestStore(t *testing.T) (*storage.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := storage.Open(dir, fakeDeviceKey(t))
	if err != nil {
		t.Fatal(err)
	}
	st.SetSelfPeerID(selfPeer)
	t.Cleanup(func() { _ = st.Close() })
	return st, dir
}

// mkRecord is a chat record helper. `to` is the peer the
// conversation is with (self is implicit).
func mkRecord(peer, dir, body string) *storage.Record {
	now := time.Date(2026, 6, 17, 10, 30, 0, 0, time.UTC)
	switch dir {
	case "out":
		return &storage.Record{
			Version:   storage.CurrentVersion,
			Timestamp: now,
			From:      selfPeer,
			To:        peer,
			Direction: "out",
			Body:      body,
			MsgID:     "0123456789abcdef",
		}
	default: // "in"
		return &storage.Record{
			Version:   storage.CurrentVersion,
			Timestamp: now,
			From:      peer,
			To:        selfPeer,
			Direction: "in",
			Body:      body,
			MsgID:     "0123456789abcdef",
		}
	}
}

// TestOpenCreatesDir verifies that Open creates the
// <saveDir>/chat/ directory (v1.1 per-peer layout) with
// the right permissions. The legacy chat.enc file is
// NOT created (migrated or empty dir, depending).
func TestOpenCreatesDir(t *testing.T) {
	dir := t.TempDir()
	st, err := storage.Open(dir, fakeDeviceKey(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	path := filepath.Join(dir, storage.ChatDirName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("chat/ not created: %v", err)
	}
	if !info.IsDir() {
		t.Errorf("%s should be a directory, got mode %s", path, info.Mode())
	}
	mode := info.Mode().Perm()
	if mode&0o700 != 0o700 {
		t.Errorf("chat/ mode = %o, want owner rwx (07xx)", mode)
	}
	// Legacy file should NOT exist on fresh install.
	if _, err := os.Stat(filepath.Join(dir, storage.FileName)); err == nil {
		t.Errorf("legacy chat.enc should not exist on fresh install")
	}
}

// TestAppendAndReadAll round-trips a few records and
// confirms ReadAll returns them in order. Records from
// multiple peers should interleave correctly (sort by
// timestamp).
func TestAppendAndReadAll(t *testing.T) {
	st, _ := newTestStore(t)

	type msg struct {
		peer, dir, body string
		ts              time.Time
	}
	now := time.Date(2026, 6, 17, 10, 30, 0, 0, time.UTC)
	msgs := []msg{
		{peerA, "out", "hi", now},
		{peerA, "in", "hey", now.Add(1 * time.Second)},
		{peerB, "out", "to B", now.Add(2 * time.Second)},
		{peerA, "in", "fine", now.Add(3 * time.Second)},
	}
	for i, m := range msgs {
		r := mkRecord(m.peer, m.dir, m.body)
		r.Timestamp = m.ts
		if err := st.Append(r); err != nil {
			t.Fatalf("Append #%d: %v", i, err)
		}
	}

	records, err := st.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if got, want := len(records), len(msgs); got != want {
		t.Fatalf("ReadAll returned %d records, want %d", got, want)
	}
	for i, want := range msgs {
		if records[i].Body != want.body {
			t.Errorf("record %d body = %q, want %q", i, records[i].Body, want.body)
		}
		if records[i].Direction != want.dir {
			t.Errorf("record %d dir = %q, want %q", i, records[i].Direction, want.dir)
		}
	}
}

// TestPerPeerFileLayout: two peers' records should land
// in two separate <chatDir>/<peerID>.enc files, not in one.
func TestPerPeerFileLayout(t *testing.T) {
	st, dir := newTestStore(t)
	_ = dir

	if err := st.Append(mkRecord(peerA, "out", "to A")); err != nil {
		t.Fatal(err)
	}
	if err := st.Append(mkRecord(peerB, "out", "to B")); err != nil {
		t.Fatal(err)
	}
	if err := st.Append(mkRecord(peerA, "in", "from A")); err != nil {
		t.Fatal(err)
	}

	chatDir := filepath.Join(st.SaveDir())
	for _, p := range []string{peerA, peerB} {
		path := filepath.Join(chatDir, storage.PeerFileName(p))
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("expected per-peer file %s, got error: %v", path, err)
			continue
		}
		if !info.Mode().IsRegular() {
			t.Errorf("%s should be a regular file", path)
		}
	}
}

// TestListPeers returns the sorted list of peer IDs that
// have at least one record.
func TestListPeers(t *testing.T) {
	st, _ := newTestStore(t)
	for _, p := range []string{peerB, peerA, peerA} {
		_ = st.Append(mkRecord(p, "out", "x"))
	}
	got, err := st.ListPeers()
	if err != nil {
		t.Fatal(err)
	}
	// Lexicographic sort: '9' (0x39) < 'a' (0x61), so
	// peerB (9988...) comes BEFORE peerA (aabb...).
	want := []string{peerB, peerA}
	if len(got) != len(want) {
		t.Fatalf("got %d peers, want %d (%v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("ListPeers[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestDeleteAllForPeer removes the per-peer file and
// makes subsequent ListPeers not see the peer. Records
// from other peers should be unaffected.
func TestDeleteAllForPeer(t *testing.T) {
	st, _ := newTestStore(t)
	for _, p := range []string{peerA, peerB} {
		_ = st.Append(mkRecord(p, "out", "x"))
	}
	if err := st.DeleteAllForPeer(peerA); err != nil {
		t.Fatalf("DeleteAllForPeer: %v", err)
	}
	peers, _ := st.ListPeers()
	if len(peers) != 1 || peers[0] != peerB {
		t.Errorf("after delete, ListPeers = %v, want [%s]", peers, peerB)
	}
	records, _ := st.ReadAll()
	for _, r := range records {
		if r.From == peerA || r.To == peerA {
			t.Errorf("found record still referencing deleted peer: %+v", r)
		}
	}
}

// TestDeleteAllForPeerIdempotent: deleting a peer that
// doesn't exist should not error (so callers can retry
// safely).
func TestDeleteAllForPeerIdempotent(t *testing.T) {
	st, _ := newTestStore(t)
	if err := st.DeleteAllForPeer(peerA); err != nil {
		t.Errorf("delete non-existent peer should be no-op, got: %v", err)
	}
}

// TestDeleteAllForPeerRejectsBadID: rejects anything
// that isn't a 32-char lowercase hex string (defends
// against path traversal via bad input).
func TestDeleteAllForPeerRejectsBadID(t *testing.T) {
	st, _ := newTestStore(t)
	for _, bad := range []string{
		"",
		"tooshort",
		"../../../etc/passwd",
		strings.Repeat("z", 32), // invalid hex
		"ABCDEF" + strings.Repeat("0", 26), // uppercase rejected
	} {
		if err := st.DeleteAllForPeer(bad); err == nil {
			t.Errorf("DeleteAllForPeer(%q) should reject", bad)
		}
	}
}

// TestReadAllFirstLaunchReturnsEmpty: when chat/
// doesn't exist or is empty, ReadAll returns (nil, nil).
func TestReadAllFirstLaunchReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	st, err := storage.Open(dir, fakeDeviceKey(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	records, err := st.ReadAll()
	if err != nil {
		t.Errorf("ReadAll: unexpected error: %v", err)
	}
	if records != nil {
		t.Errorf("ReadAll on first launch: got %d records, want nil", len(records))
	}
}

// TestWrongKeyFailsDecrypt: a per-peer file encrypted
// with key A can't be read with key B. The per-peer
// layout tolerates per-file corruption (skips the bad
// file and reads the rest) so the result is partial,
// not a total failure.
func TestWrongKeyFailsDecrypt(t *testing.T) {
	dir := t.TempDir()
	keyA := fakeDeviceKey(t)

	stA, err := storage.Open(dir, keyA)
	if err != nil {
		t.Fatal(err)
	}
	stA.SetSelfPeerID(selfPeer)
	if err := stA.Append(mkRecord(peerA, "out", "secret")); err != nil {
		t.Fatal(err)
	}
	if err := stA.Close(); err != nil {
		t.Fatal(err)
	}

	keyB := fakeDeviceKey(t)
	stB, err := storage.Open(dir, keyB)
	if err != nil {
		t.Fatal(err)
	}
	defer stB.Close()
	// Per-peer layout: ReadAll skips the corrupt peer
	// file silently (returns no error, no record). The
	// legacy "stop at first corruption" semantic would
	// have been too aggressive — one bad peer would
	// hide every other peer's history.
	records, err := stB.ReadAll()
	if err != nil {
		t.Errorf("ReadAll with wrong key: unexpected error: %v", err)
	}
	if len(records) != 0 {
		t.Errorf("ReadAll with wrong key should see no records, got %d", len(records))
	}
}

// TestAppendAfterCloseReturnsErrClosed: Close makes
// subsequent Appends fail with ErrClosed, not panic.
func TestAppendAfterCloseReturnsErrClosed(t *testing.T) {
	st, _ := newTestStore(t)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	err := st.Append(mkRecord(peerA, "out", "post-close"))
	if err == nil {
		t.Error("Append after Close should return an error")
	}
	if !strings.Contains(err.Error(), "closed") {
		t.Errorf("expected 'closed' in error, got %v", err)
	}
}

// TestCloseIsIdempotent: calling Close twice is a no-op.
func TestCloseIsIdempotent(t *testing.T) {
	st, _ := newTestStore(t)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Errorf("second Close returned error: %v", err)
	}
}

// TestConcurrentAppendsAreSerialized: many goroutines
// Appending to the same peer should produce a valid
// per-peer file that ReadAll can decode (no interleaved
// frames). Concurrent Appends to DIFFERENT peers should
// also work (real disk parallelism; each peer gets its
// own handle).
func TestConcurrentAppendsAreSerialized(t *testing.T) {
	st, _ := newTestStore(t)

	const goroutines = 10
	const perGoroutine = 5
	done := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		go func(id int) {
			for j := 0; j < perGoroutine; j++ {
				// Half the goroutines write to peerA,
				// the other half to peerB, exercising
				// both the "same peer" and "different
				// peer" concurrency paths.
				p := peerA
				if id%2 == 1 {
					p = peerB
				}
				body := "g" + string(rune('A'+id)) + "-" + string(rune('0'+j))
				_ = st.Append(mkRecord(p, "out", body))
			}
			done <- struct{}{}
		}(g)
	}
	for g := 0; g < goroutines; g++ {
		<-done
	}
	records, err := st.ReadAll()
	if err != nil {
		t.Fatalf("ReadAll after concurrent Append: %v", err)
	}
	if got, want := len(records), goroutines*perGoroutine; got != want {
		t.Errorf("got %d records, want %d", got, want)
	}
}

// TestAppendNilRecordRejected.
func TestAppendNilRecordRejected(t *testing.T) {
	st, _ := newTestStore(t)
	defer st.Close()
	if err := st.Append(nil); err == nil {
		t.Error("Append(nil) should return an error")
	}
}

// TestAppendBeforeSetSelfPeerID: refusing to guess
// prevents putting a record in the wrong per-peer file
// (which would be silent corruption).
func TestAppendBeforeSetSelfPeerID(t *testing.T) {
	dir := t.TempDir()
	st, err := storage.Open(dir, fakeDeviceKey(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	err = st.Append(mkRecord(peerA, "out", "before self"))
	if err == nil {
		t.Error("Append before SetSelfPeerID should return an error")
	}
	if !strings.Contains(err.Error(), "SetSelfPeerID") {
		t.Errorf("expected 'SetSelfPeerID' in error, got %v", err)
	}
}

// TestOpenRejectsBadKeyLength.
func TestOpenRejectsBadKeyLength(t *testing.T) {
	dir := t.TempDir()
	_, err := storage.Open(dir, []byte("too short"))
	if err == nil {
		t.Error("Open with short key should fail")
	}
}

// TestFrameRoundTrip: encode/encrypt/write/read/decrypt/decode
// end to end.
func TestFrameRoundTrip(t *testing.T) {
	st, _ := newTestStore(t)
	r := mkRecord(peerA, "out", "frame round trip")
	if err := st.Append(r); err != nil {
		t.Fatal(err)
	}
	records, err := st.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Body != r.Body {
		t.Errorf("frame round-trip failed: %+v", records)
	}
}

// TestEmptyBody.
func TestEmptyBody(t *testing.T) {
	st, _ := newTestStore(t)
	if err := st.Append(mkRecord(peerA, "out", "")); err != nil {
		t.Fatal(err)
	}
	records, err := st.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Body != "" {
		t.Errorf("empty body round-trip failed: %+v", records)
	}
}

// TestUnicodeBody.
func TestUnicodeBody(t *testing.T) {
	st, _ := newTestStore(t)
	body := "浣犲ソ锛屼笘鐣岋紒馃殌 Mavis is here."
	if err := st.Append(mkRecord(peerA, "out", body)); err != nil {
		t.Fatal(err)
	}
	records, err := st.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Body != body {
		t.Errorf("unicode round-trip failed:\n  got  %q\n  want %q",
			records[0].Body, body)
	}
}

// TestLargeBody: 64 KiB body is comfortably below the
// 1 MiB max-frame sanity bound.
func TestLargeBody(t *testing.T) {
	st, _ := newTestStore(t)
	body := strings.Repeat("x", 64*1024)
	if err := st.Append(mkRecord(peerA, "out", body)); err != nil {
		t.Fatal(err)
	}
	records, err := st.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Body != body {
		t.Errorf("large body round-trip failed")
	}
}