package group

import (
	"bytes"
	"testing"
	"time"

	"github.com/weishengsuptp/innerlink/internal/crypto"
)

// makeInvite constructs a signed Invite for tests, returning the
// Invite, the inviter's key pair, and the underlying GroupID.
func makeInvite(t *testing.T) (*Invite, *crypto.SM2PrivateKey, []byte) {
	t.Helper()
	priv, err := crypto.GenerateSM2Key()
	if err != nil {
		t.Fatalf("GenerateSM2Key: %v", err)
	}
	creator := []byte("creator-peer-id-16b")
	now := time.Date(2026, 6, 27, 22, 0, 0, 0, time.UTC)
	gid := ComputeGroupID(creator, "test group", now)
	inv := NewInvite(gid, "test group", "creator-peer-id", "inviter-peer-id", now)
	if err := inv.Sign(priv); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// Sanity: the rendered GroupID matches the hash.
	if inv.GroupID != RenderGroupID(gid) {
		t.Errorf("Invite.GroupID = %q, want %q", inv.GroupID, RenderGroupID(gid))
	}
	return inv, priv, gid
}

// pub returns the *SM2PublicKey matching priv. We can't use
// priv.Public() directly because gmsm types it as
// crypto.PublicKey (the stdlib interface), not the concrete
// *SM2PublicKey we need for Verify.
func pub(priv *crypto.SM2PrivateKey) *crypto.SM2PublicKey {
	return &priv.PublicKey
}

func TestInviteSignVerifyRoundtrip(t *testing.T) {
	inv, priv, _ := makeInvite(t)
	if err := inv.Verify(pub(priv)); err != nil {
		t.Errorf("Verify after Sign: %v", err)
	}
}

func TestInviteVerifyWrongPubFails(t *testing.T) {
	inv, _, _ := makeInvite(t)
	wrong, _ := crypto.GenerateSM2Key()
	if err := inv.Verify(pub(wrong)); err == nil {
		t.Error("Verify accepted wrong public key")
	}
}

func TestInviteVerifyNilPubFails(t *testing.T) {
	inv, _, _ := makeInvite(t)
	if err := inv.Verify(nil); err == nil {
		t.Error("Verify(nil) accepted")
	}
}

func TestInviteVerifyMissingSignatureFails(t *testing.T) {
	priv, _ := crypto.GenerateSM2Key()
	inv := NewInvite([]byte("creator-peer"), "g", "c", "i", time.Now())
	// Not signed.
	if err := inv.Verify(pub(priv)); err == nil {
		t.Error("Verify accepted unsigned invite")
	}
}

func TestInviteTamperDetected(t *testing.T) {
	inv, priv, _ := makeInvite(t)
	// Change one field after signing — verification should fail.
	inv.GroupName = "tampered name"
	if err := inv.Verify(pub(priv)); err == nil {
		t.Error("Verify accepted tampered GroupName")
	}
	inv.GroupName = "test group" // restore
	if err := inv.Verify(pub(priv)); err != nil {
		t.Errorf("Verify after restore: %v", err)
	}
}

func TestInviteNonceUnique(t *testing.T) {
	// Two NewInvite calls with the same parameters produce
	// different nonces (otherwise a recipient's replay defense
	// would dedupe a fresh invite as a replay).
	priv, _ := crypto.GenerateSM2Key()
	gid := []byte("creator-peer")
	t0 := time.Date(2026, 6, 27, 22, 0, 0, 0, time.UTC)
	inv1 := NewInvite(gid, "g", "c", "i", t0)
	inv2 := NewInvite(gid, "g", "c", "i", t0)
	_ = inv1.Sign(priv)
	_ = inv2.Sign(priv)
	if bytes.Equal(inv1.Nonce, inv2.Nonce) {
		t.Error("two NewInvite calls produced identical nonces")
	}
	if len(inv1.Nonce) != 16 {
		t.Errorf("Nonce len = %d, want 16", len(inv1.Nonce))
	}
}

func TestInviteExpiry(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour)
	inv := NewInvite([]byte("x"), "g", "c", "i", old)
	if inv.Expiry(time.Hour) != true {
		t.Error("Expiry(1h) = false for 2h-old invite; want true")
	}
	if inv.Expiry(0) != false {
		t.Error("Expiry(0) = true; want false (maxAge=0 means never expire)")
	}
	if inv.Expiry(24*time.Hour) != false {
		t.Error("Expiry(24h) = true for 2h-old invite; want false")
	}
}
