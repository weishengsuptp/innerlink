package group

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/weishengsuptp/innerlink/internal/crypto"
)

// ChainKeySize is the SM3 output size (256 bits = 32 bytes).
// The chain key is fed into KDF-Expand (per-sender KDF context)
// to derive:
//
//   - the per-message SM4-GCM data key (for content encryption)
//   - the next chain key (for ratcheting forward)
//
// Why 32 bytes (not 16 like SM4): SM3 output is 32 bytes, and
// using the raw SM3 digest as the chain key means Advance() is
// a one-shot SM3 call with no truncation. KDF-Expand downstream
// can take any input length.
//
// Forward secrecy: after Advance, the old chain key MUST be
// zeroed by the caller. Losing the new key doesn't decrypt older
// messages; losing the old key doesn't decrypt newer ones.
const ChainKeySize = 32

// ChainIndex is the position of a message in a sender's chain.
// Sender's first message in the group uses index 0; each
// Advance() call bumps the index. Recipients that detect a gap
// (their local index < sender's announced index - 1) request a
// resync.
type ChainIndex uint64

// SenderKey is one sender's current chain key for one group.
// Stored at <dataDir>/groups/<renderedID>/sender-keys/<peerID>.key
// (the storage layer adds the file I/O; this struct is the
// in-memory and on-wire form).
//
// Why one SenderKey per (group, sender):
//
//   - Sender Keys (Signal-style, a.k.a. Megaolm): each sender
//     ratchets their own chain independently. Other members hold
//     a copy of each sender's current chain key.
//   - Removing a member only requires re-keying the OUTGOING
//     chains of every remaining sender (one message per member);
//     each sender's chain ratchets on every send, so even without
//     re-keying the captured old chain has limited decryptability.
//   - The alternative (Signal-style Double Ratchet, a la 1:1
//     Signal sessions) is much heavier per group and not
//     necessary at our <20-person group scale.
type SenderKey struct {
	Version    int        `json:"v"`              // SenderKey wire format version (always 1)
	GroupID    string     `json:"g"`              // rendered "g_<hex>"
	SenderID   string     `json:"s"`              // sender's peerID
	ChainKey   []byte     `json:"k"`              // ChainKeySize bytes; current chain key
	ChainIndex ChainIndex `json:"i"`              // current chain index
	IssuedAt   string     `json:"t"`              // RFC3339Nano UTC
	Signature  []byte     `json:"sig"`            // SM2(senderPriv, canonical); binds the sender's identity
}

// Canonical form used for signing — same approach as Invite:
// fixed field order, short keys. Decoupling the wire shape from
// the signed shape lets us add optional fields later without
// invalidating old signatures (we'd add to the optional bucket).
type senderKeyCanonical struct {
	GroupID    string     `json:"g"`
	SenderID   string     `json:"s"`
	ChainKey   []byte     `json:"k"`
	ChainIndex ChainIndex `json:"i"`
	IssuedAt   string     `json:"t"`
}

func (sk *SenderKey) toCanonical() []byte {
	c := senderKeyCanonical{
		GroupID:    sk.GroupID,
		SenderID:   sk.SenderID,
		ChainKey:   sk.ChainKey,
		ChainIndex: sk.ChainIndex,
		IssuedAt:   sk.IssuedAt,
	}
	// Same marshal-can't-fail reasoning as Invite.toCanonical.
	b, _ := json.Marshal(c)
	return b
}

// NewSenderKey allocates a fresh chain key for (group, sender)
// and signs the whole envelope with senderPriv. The first chain
// key is purely random; subsequent ratcheting uses Advance.
//
// IssuedAt should be time.Now().UTC().Format(time.RFC3339Nano)
// (caller passes the formatted string for testability).
func NewSenderKey(rawGroupID []byte, senderID string, senderPriv *crypto.SM2PrivateKey, issuedAt string) (*SenderKey, error) {
	chain, err := newRandomChainKey()
	if err != nil {
		return nil, err
	}
	sk := &SenderKey{
		Version:    1,
		GroupID:    RenderGroupID(rawGroupID),
		SenderID:   senderID,
		ChainKey:   chain,
		ChainIndex: 0,
		IssuedAt:   issuedAt,
	}
	sig, err := crypto.SignSM2(senderPriv, sk.toCanonical())
	if err != nil {
		return nil, fmt.Errorf("group: SenderKey sign: %w", err)
	}
	sk.Signature = sig
	return sk, nil
}

// newRandomChainKey returns ChainKeySize bytes from the RNG.
// Wrapped so tests can mock the RNG.
func newRandomChainKey() ([]byte, error) {
	k := make([]byte, ChainKeySize)
	if _, err := cryptoRandRead(k); err != nil {
		return nil, fmt.Errorf("group: random chain key: %w", err)
	}
	return k, nil
}

// Verify checks that Signature was produced by senderPub over the
// canonical form. Returns nil on success.
func (sk *SenderKey) Verify(senderPub *crypto.SM2PublicKey) error {
	if senderPub == nil {
		return errors.New("group: SenderKey verify: nil pub")
	}
	if len(sk.Signature) == 0 {
		return errors.New("group: SenderKey verify: missing signature")
	}
	if !crypto.VerifySM2(senderPub, sk.toCanonical(), sk.Signature) {
		return errors.New("group: SenderKey verify: signature invalid")
	}
	return nil
}

// Advance returns the next SenderKey in the ratchet: chain key
// is replaced by SM3("innerlink-group-chain-v1" || oldChainKey),
// index is incremented, the original (sk) is unchanged, and the
// returned SenderKey carries a freshly-signature over the new
// canonical form.
//
// The old chain key MUST be zeroed out by the caller after this
// returns (forward secrecy — losing the new key should not
// decrypt older messages, and losing the old key should not
// decrypt newer ones).
//
// Why SM3 not a full KDF: chain-key ratcheting is just "next
// secret" derivation, not "extract-and-expand"; SM3(input) is
// already a PRF for arbitrary-length inputs. Domain-separated by
// the leading constant so chain keys can never be confused with
// other SM3 outputs (file SHA-256s, PeerIDs, etc.).
func Advance(sk *SenderKey, senderPriv *crypto.SM2PrivateKey) (*SenderKey, error) {
	if sk == nil {
		return nil, errors.New("group: Advance: nil SenderKey")
	}
	if len(sk.ChainKey) != ChainKeySize {
		return nil, fmt.Errorf("group: Advance: bad chain key size %d, want %d",
			len(sk.ChainKey), ChainKeySize)
	}
	nextKey := advanceChainKey(sk.ChainKey)
	next := &SenderKey{
		Version:    sk.Version,
		GroupID:    sk.GroupID,
		SenderID:   sk.SenderID,
		ChainKey:   nextKey,
		ChainIndex: sk.ChainIndex + 1,
		IssuedAt:   sk.IssuedAt,
	}
	sig, err := crypto.SignSM2(senderPriv, next.toCanonical())
	if err != nil {
		return nil, fmt.Errorf("group: Advance sign: %w", err)
	}
	next.Signature = sig
	return next, nil
}

// advanceChainKey = SM3("innerlink-group-chain-v1" || oldKey).
// Exposed as a separate function so tests can assert the ratchet
// is deterministic when seeded with known inputs.
func advanceChainKey(oldKey []byte) []byte {
	h := crypto.SM3()
	h.Write([]byte("innerlink-group-chain-v1"))
	h.Write(oldKey)
	return h.Sum(nil)
}
