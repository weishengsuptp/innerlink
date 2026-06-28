package storage

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"

	ic "github.com/weishengsuptp/innerlink/internal/crypto"
)

// This file collects the SM4-CBC frame read/write helpers that
// both Append (per-peer) and AppendGroup (per-group) need.
// Originally inlined in store.go Append; lifted out so the
// group code path doesn't duplicate the encryption / framing
// logic.
//
// Frame format (identical for per-peer chat.enc and per-group
// chat.enc):
//
//	+----------+----------------+--------------------+
//	| 4B lenBE |  16B IV (rand) |   ciphertext       |
//	+----------+----------------+--------------------+
//	\_____________  _____________________________/
//	              \/
//	    SM4-CBC(device-key, iv, JSON(Record))
//
// Length field is the byte length of the CIPHERTEXT (post-
// SM4-CBC padding), not the plaintext. AES-CBC + SM4-CBC are
// both PKCS#7-padded, so the length of CT = 16*ceil(PT/16).
//
// maxFrame is 1 MiB — a single record can't legitimately
// exceed this (chat lines are short, even file messages
// store only the path, not the bytes themselves).
const maxFrame = 1 << 20

// writeFrame encodes rec, encrypts with key, and appends
// one frame to path. Atomically opens for append; the file
// is created if missing.
//
// This is the "lower half" of the legacy chat.enc Append
// path; we use the same wire format for per-group chat.enc
// (v1.1) so a single decoder works on both.
func writeFrame(path string, key []byte, rec *Record) error {
	if rec.Version == 0 {
		rec.Version = CurrentVersion
	}
	plain, err := rec.encode()
	if err != nil {
		return fmt.Errorf("storage: encode record: %w", err)
	}
	iv, err := ic.NewNonce(FrameIVSize)
	if err != nil {
		return fmt.Errorf("storage: generate IV: %w", err)
	}
	ct, err := ic.SM4EncryptCBC(key, iv, plain)
	if err != nil {
		return fmt.Errorf("storage: encrypt record: %w", err)
	}
	frame := make([]byte, 0, FrameHeaderSize+FrameIVSize+len(ct))
	var lenBuf [FrameHeaderSize]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(ct)))
	frame = append(frame, lenBuf[:]...)
	frame = append(frame, iv...)
	frame = append(frame, ct...)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, FileMode)
	if err != nil {
		return fmt.Errorf("storage: open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(frame); err != nil {
		return fmt.Errorf("storage: write %s: %w", path, err)
	}
	return nil
}

// readAllBytes decodes every record from b (a single
// chat.enc's full contents) and returns them. Same wire
// format as writeFrame; the legacy stream-style readAll in
// store.go has the same logic but operates on an io.Reader.
// readAllBytes is the []byte equivalent for one-shot loads
// (used by HistoryGroup).
func readAllBytes(b []byte, key []byte) ([]*Record, error) {
	var recs []*Record
	br := bytes.NewReader(b)
	for {
		var lenBuf [FrameHeaderSize]byte
		readN, rerr := io.ReadFull(br, lenBuf[:])
		if readN == 0 && rerr == io.EOF {
			return recs, nil
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			if len(recs) == 0 {
				return recs, nil
			}
			return nil, ErrCorrupt
		}
		if rerr != nil {
			return nil, fmt.Errorf("storage: read header: %w", rerr)
		}
		frameLen := binary.BigEndian.Uint32(lenBuf[:])
		if frameLen == 0 || frameLen > maxFrame {
			return nil, ErrCorrupt
		}
		frame := make([]byte, FrameIVSize+int(frameLen))
		readN, rerr = io.ReadFull(br, frame)
		if rerr != nil {
			return nil, fmt.Errorf("storage: read frame body: %w", rerr)
		}
		iv := frame[:FrameIVSize]
		ct := frame[FrameIVSize:]
		plain, derr := ic.SM4DecryptCBC(key, iv, ct)
		if derr != nil {
			return nil, ErrCorrupt
		}
		rec, derr := decodeRecord(plain)
		if derr != nil {
			return nil, fmt.Errorf("storage: decode record: %w", derr)
		}
		recs = append(recs, rec)
	}
}
