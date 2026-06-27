package storage_test

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ic "github.com/weishengsuptp/innerlink/internal/crypto"
	"github.com/weishengsuptp/innerlink/internal/storage"
)

// writeLegacyChat writes a v0.5-v1.0 style chat.enc into
// dir using the SAME per-record framing as the legacy
// Store.Append. Used by migration tests.
//
// selfID is the device's own PeerID (32 hex). The
// returned records[] is what's written so the test can
// assert on them after migration.
func writeLegacyChat(t *testing.T, dir string, key []byte, selfID string, items []legacyItem) []storage.Record {
	t.Helper()
	sm4Key, err := ic.KDF(key, []byte("innerlink-storage-v1"), storage.KeySize)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, storage.FileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, storage.FileMode)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var out []storage.Record
	for i, it := range items {
		rec := storage.Record{
			Version:   storage.CurrentVersion,
			Timestamp: it.ts,
			From:      it.from,
			To:        it.to,
			Direction: it.dir,
			Body:      it.body,
			MsgID:     "0123456789abcdef",
		}
		plain, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		iv, err := ic.NewNonce(storage.FrameIVSize)
		if err != nil {
			t.Fatal(err)
		}
		ct, err := ic.SM4EncryptCBC(sm4Key, iv, plain)
		if err != nil {
			t.Fatal(err)
		}
		var lenBuf [storage.FrameHeaderSize]byte
		binary.BigEndian.PutUint32(lenBuf[:], uint32(len(ct)))
		if _, err := f.Write(lenBuf[:]); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(iv); err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(ct); err != nil {
			t.Fatal(err)
		}
		out = append(out, rec)
		_ = i
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	_ = selfID
	return out
}

type legacyItem struct {
	from, to, dir, body string
	ts                  time.Time
}

// TestMigrationFromLegacyChatEnc: a v1.0 chat.enc is
// auto-migrated to per-peer <chat>/<peerID>.enc on first
// Open, and the legacy file is renamed to chat.enc.migrated-*.
func TestMigrationFromLegacyChatEnc(t *testing.T) {
	dir := t.TempDir()
	key := fakeDeviceKey(t)
	selfID := selfPeer

	// Build legacy chat.enc with 4 records: 3 with peerA,
	// 1 with peerB. Both directions, alternating.
	t0 := time.Date(2026, 6, 17, 10, 30, 0, 0, time.UTC)
	expected := writeLegacyChat(t, dir, key, selfID, []legacyItem{
		{from: selfID, to: peerA, dir: "out", body: "hi A", ts: t0},
		{from: peerA, to: selfID, dir: "in", body: "hey", ts: t0.Add(1 * time.Second)},
		{from: selfID, to: peerB, dir: "out", body: "hi B", ts: t0.Add(2 * time.Second)},
		{from: selfID, to: peerA, dir: "out", body: "and again", ts: t0.Add(3 * time.Second)},
	})

	// Confirm legacy file exists pre-migration.
	if _, err := os.Stat(filepath.Join(dir, storage.FileName)); err != nil {
		t.Fatalf("legacy chat.enc not present: %v", err)
	}

	// Open triggers migration.
	st, err := storage.Open(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	st.SetSelfPeerID(selfID)
	defer st.Close()

	// After migration:
	//   1. Legacy file is gone (renamed to backup).
	if _, err := os.Stat(filepath.Join(dir, storage.FileName)); err == nil {
		t.Errorf("legacy chat.enc should be renamed after migration")
	}
	//   2. Per-peer files exist for both peers.
	for _, p := range []string{peerA, peerB} {
		path := filepath.Join(st.SaveDir(), storage.PeerFileName(p))
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected per-peer file for %s: %v", p, err)
		}
	}
	//   3. A backup file with chat.enc.migrated-* prefix exists.
	entries, _ := os.ReadDir(dir)
	var foundBackup bool
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "chat.enc.migrated-") {
			foundBackup = true
			break
		}
	}
	if !foundBackup {
		t.Errorf("expected chat.enc.migrated-* backup file in %s, got %v", dir, entries)
	}

	//   4. ReadAll returns the same 4 records (same
	//      bodies, in timestamp order).
	got, err := st.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(expected) {
		t.Fatalf("ReadAll after migration: got %d records, want %d", len(got), len(expected))
	}
	for i, want := range expected {
		if got[i].Body != want.Body {
			t.Errorf("record %d body = %q, want %q", i, got[i].Body, want.Body)
		}
		if got[i].Direction != want.Direction {
			t.Errorf("record %d dir = %q, want %q", i, got[i].Direction, want.Direction)
		}
	}
}

// TestMigrationIsIdempotent: a second Open on an
// already-migrated data dir is a no-op (doesn't rename
// anything, doesn't lose any records).
func TestMigrationIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	key := fakeDeviceKey(t)

	t0 := time.Date(2026, 6, 17, 10, 30, 0, 0, time.UTC)
	writeLegacyChat(t, dir, key, selfPeer, []legacyItem{
		{from: selfPeer, to: peerA, dir: "out", body: "hi A", ts: t0},
	})

	// First Open: migrates.
	st1, err := storage.Open(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	st1.SetSelfPeerID(selfPeer)
	if err := st1.Close(); err != nil {
		t.Fatal(err)
	}

	// Capture file listing after first migration.
	entries1, _ := os.ReadDir(dir)

	// Second Open: should be a no-op.
	st2, err := storage.Open(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	st2.SetSelfPeerID(selfPeer)
	defer st2.Close()
	entries2, _ := os.ReadDir(dir)

	if len(entries1) != len(entries2) {
		t.Errorf("second Open mutated directory: before %d entries, after %d", len(entries1), len(entries2))
	}

	// Records should still be readable.
	got, err := st2.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Body != "hi A" {
		t.Errorf("ReadAll after re-Open: got %+v, want [hi A]", got)
	}
}

// TestFreshInstallDoesNotMigrate: an empty data dir has
// no chat.enc, so Open skips migration and just creates
// the chat/ directory.
func TestFreshInstallDoesNotMigrate(t *testing.T) {
	dir := t.TempDir()
	st, err := storage.Open(dir, fakeDeviceKey(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := os.Stat(filepath.Join(dir, storage.FileName)); err == nil {
		t.Errorf("fresh install should not create chat.enc")
	}
	if _, err := os.Stat(filepath.Join(dir, storage.ChatDirName)); err != nil {
		t.Errorf("fresh install should create chat/: %v", err)
	}
}

// keep rand referenced for future tests in this file.
var _ = rand.Reader