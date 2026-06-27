package group

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/weishengsuptp/innerlink/internal/crypto"
)

// Invite is a 1:1 message from a current group member to a
// prospective member, asking them to join a group. The recipient
// can Verify() the signature against the inviter's SM2 public
// key (received during the original handshake) before deciding
// whether to accept.
//
// Why SM2-sign the invite (rather than just trusting it):
//
//   - Without a signature, any peer could forge an invite for
//     any group ("join this group, here's the sender key for
//     every member"), and the recipient would hand over their
//     sender key material to the attacker.
//   - The inviter's SM2 public key is already known to the
//     recipient via the handshake layer (every channel is
//     authenticated end-to-end). No key distribution needed.
//
// Lifecycle:
//
//   - Inviter calls NewInvite(...) then Sign(priv)
//   - Inviter sends Invite in a 1:1 channel as the payload of a
//     TypeInvite envelope (next envelope type after v2 freezes;
//     see GROUP-DESIGN.md §3 for the planned addition).
//   - Recipient calls Verify(inviterPub); if it fails, drop.
//   - On accept: recipient sends back TypeInviteAccept; inviter
//     adds recipient to members.json and starts distributing
//     sender keys (see senderkey.go).
//   - On decline: recipient sends back TypeInviteDecline (or
//     simply ignores — both are valid).
type Invite struct {
	GroupID   string    `json:"group_id"`            // rendered "g_<hex>"
	GroupName string    `json:"group_name"`
	Creator   string    `json:"creator"`             // creator's peerID
	Inviter   string    `json:"inviter"`             // whoever sent THIS invite
	IssuedAt  time.Time `json:"issued_at"`           // when the inviter created it
	Nonce     []byte    `json:"nonce,omitempty"`     // 16 bytes, replay defense
	Signature []byte    `json:"signature,omitempty"` // SM2 over the canonical form
}

// canonicalFields is the fixed-order, fixed-name projection of
// Invite used as the SM2 message. Defining a separate struct
// (rather than marshalling the public Invite struct) means:
//
//   - Adding a new field to Invite later (e.g. expiry) doesn't
//     silently change the signature unless we explicitly include
//     it here. The compile error is the desired safety net.
//   - Field order is JSON-marshalling-deterministic in Go's
//     encoding/json (sorted by struct declaration order), but
//     short single-letter keys ("g", "n", "c", "i", "t") also
//     make the canonical blob shorter for in-memory use.
type canonicalFields struct {
	GroupID   string `json:"g"`
	GroupName string `json:"n"`
	Creator   string `json:"c"`
	Inviter   string `json:"i"`
	IssuedAt  string `json:"t"` // RFC3339Nano UTC, normalized
	Nonce     []byte `json:"o"` // base64 in JSON, raw in memory
}

// toCanonical returns the byte form that SM2 signs / verifies.
// IssuedAt is normalized to UTC + RFC3339Nano so two peers in
// different timezones produce identical signatures.
func (inv *Invite) toCanonical() []byte {
	cf := canonicalFields{
		GroupID:   inv.GroupID,
		GroupName: inv.GroupName,
		Creator:   inv.Creator,
		Inviter:   inv.Inviter,
		IssuedAt:  inv.IssuedAt.UTC().Format(time.RFC3339Nano),
		Nonce:     inv.Nonce,
	}
	// json.Marshal on a struct with only string + []byte fields
	// never fails (no NaN/Inf, no unsupported types), so we
	// ignore the error deliberately. If we ever add a field
	// that can fail to marshal, this needs to surface that.
	b, _ := json.Marshal(cf)
	return b
}

// NewInvite constructs an unsigned Invite. Caller must call
// Sign(priv) before sending. IssuedAt is set to time.Now().UTC();
// callers wanting a fixed timestamp (for tests) should set it
// explicitly after construction.
func NewInvite(rawGroupID []byte, groupName, creator, inviter string, t time.Time) *Invite {
	return &Invite{
		GroupID:   RenderGroupID(rawGroupID),
		GroupName: groupName,
		Creator:   creator,
		Inviter:   inviter,
		IssuedAt:  t,
		// 16-byte random nonce — the recipient checks it hasn't
		// seen this exact (Inviter, Nonce) pair recently to
		// reject replays of a captured invite.
		Nonce: randomNonce(16),
	}
}

// randomNonce returns n cryptographically-random bytes. Errors
// from crypto/rand are extremely rare (only on a broken OS RNG)
// and propagate up via the panic — the alternative is silently
// producing predictable nonces which is much worse.
func randomNonce(n int) []byte {
	b := make([]byte, n)
	if _, err := cryptoRandRead(b); err != nil {
		panic(fmt.Sprintf("group: randomNonce: %v", err))
	}
	return b
}

// Sign fills in Invite.Signature using priv. Returns an error
// only if SM2 itself fails (key corruption, RNG failure).
func (inv *Invite) Sign(priv *crypto.SM2PrivateKey) error {
	sig, err := crypto.SignSM2(priv, inv.toCanonical())
	if err != nil {
		return fmt.Errorf("group: invite sign: %w", err)
	}
	inv.Signature = sig
	return nil
}

// Verify checks the signature against pub. Returns nil on
// success or one of:
//
//   - "missing signature" — caller passed an Invite without Sign
//   - "signature invalid" — pub doesn't match, or signature was
//     tampered, or canonical form differs (different Invite
//     version, different IssuedAt formatting, etc.)
//
// Verify is safe to call on a zero-value Invite (returns
// "missing signature").
func (inv *Invite) Verify(pub *crypto.SM2PublicKey) error {
	if pub == nil {
		return errors.New("group: invite verify: nil pub")
	}
	if len(inv.Signature) == 0 {
		return errors.New("group: invite verify: missing signature")
	}
	if !crypto.VerifySM2(pub, inv.toCanonical(), inv.Signature) {
		return errors.New("group: invite verify: signature invalid")
	}
	return nil
}

// Expiry reports whether the invite is older than maxAge. Used
// by the recipient to reject invites that have been replaying
// around the network for too long. maxAge=0 means "never expire"
// (useful for tests).
func (inv *Invite) Expiry(maxAge time.Duration) bool {
	if maxAge <= 0 {
		return false
	}
	return time.Since(inv.IssuedAt) > maxAge
}
