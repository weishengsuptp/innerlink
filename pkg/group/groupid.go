package group

import (
	"encoding/hex"
	"fmt"
	"hash"
	"time"

	"github.com/weishengsuptp/innerlink/internal/crypto"
)

// RawGroupIDSize is the SM3 output size for a GroupID.
const RawGroupIDSize = 32

// RenderedGroupIDSize is the length of the human-friendly
// "g_<64hex>" form (2 char prefix + 64 hex chars).
// hex.EncodedLen is a function so it can't appear in a const
// expression; we hard-code 2+64=66 and add a build-time check
// against hex.EncodedLen in the test file.
const RenderedGroupIDSize = 66

// ComputeGroupID returns the SM3 hash of (creatorPeerID || groupName ||
// creationUnixNanos). The hash is 32 raw bytes — call RenderGroupID to
// produce the human-readable "g_<hex>" form for logs and on-disk
// directory names.
//
// Why content-addressed:
//
//   - Two peers who independently decide "Alice creates 'project team'
//     at this exact nanosecond" arrive at the same GroupID without
//     any coordination. No registration authority needed.
//   - SM3 collision probability is 2^-128 for any realistic group
//     count, so accidental collisions don't happen.
//   - Storing the GroupID is enough to reproduce all metadata — we
//     don't need to also store the inputs.
//
// The exact inputs are NOT included in the ID (only hashed in), so a
// peer who learns a GroupID cannot recover the creator's PeerID or
// the group name from it alone. They need a separate members.json or
// invite to learn those.
func ComputeGroupID(creatorPeerID []byte, groupName string, t time.Time) []byte {
	h := crypto.SM3()
	h.Write(creatorPeerID)
	h.Write([]byte(groupName))
	// Big-endian uint64 nanoseconds since the unix epoch.
	// UnixNano is monotonic within a single Go process, but we don't
	// require monotonicity across peers — each peer's local clock
	// is fine for ID generation; the ID just needs to be unique
	// enough within the creator's intent.
	//
	// Cast through int64 to avoid the uint64 sign-extension trap:
	// time.Time.UnixNano returns int64, and a future date past 2262
	// would overflow int64 → negative int64 → wrong hash. The cast
	// keeps the bits identical.
	bePutUint64(h, uint64(t.UnixNano()))
	return h.Sum(nil)
}

// bePutUint64 writes 8 bytes of v into h in big-endian order.
// Equivalent to binary.BigEndian.PutUint64 but doesn't allocate
// the [8]byte scratch buffer on every call.
func bePutUint64(h hash.Hash, v uint64) {
	var b [8]byte
	b[0] = byte(v >> 56)
	b[1] = byte(v >> 48)
	b[2] = byte(v >> 40)
	b[3] = byte(v >> 32)
	b[4] = byte(v >> 24)
	b[5] = byte(v >> 16)
	b[6] = byte(v >> 8)
	b[7] = byte(v)
	h.Write(b[:])
}

// RenderGroupID returns "g_<64hex>" for the raw 32-byte GroupID.
// Use this for log lines, directory names, and the on-disk
// members.json's GroupID field. The raw bytes are used on the wire
// (Envelope's GroupID field) and inside SM3 inputs.
func RenderGroupID(raw []byte) string {
	return "g_" + hex.EncodeToString(raw)
}

// ParseGroupID accepts both "g_<64hex>" (the rendered form) and
// bare 64-hex (no prefix). Returns the raw 32 bytes. Used to parse
// GroupIDs from CLI args, JSON, or log lines.
func ParseGroupID(s string) ([]byte, error) {
	if len(s) == RenderedGroupIDSize && s[:2] == "g_" {
		return hex.DecodeString(s[2:])
	}
	if len(s) == hex.EncodedLen(RawGroupIDSize) {
		return hex.DecodeString(s)
	}
	return nil, fmt.Errorf("group: bad GroupID %q (want g_<64hex> or <64hex>)", s)
}
