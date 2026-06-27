package group

import (
	"bytes"
	"testing"
	"time"

	"github.com/weishengsuptp/innerlink/internal/crypto"
)

func makeSenderKey(t *testing.T) (*SenderKey, *crypto.SM2PrivateKey, []byte) {
	t.Helper()
	priv, err := crypto.GenerateSM2Key()
	if err != nil {
		t.Fatalf("GenerateSM2Key: %v", err)
	}
	gid := ComputeGroupID([]byte("creator-peer"), "test", time.Now())
	now := time.Now().UTC().Format(time.RFC3339Nano)
	sk, err := NewSenderKey(gid, "sender-peer-id", priv, now)
	if err != nil {
		t.Fatalf("NewSenderKey: %v", err)
	}
	return sk, priv, gid
}

func TestSenderKeyInitialState(t *testing.T) {
	sk, _, _ := makeSenderKey(t)
	if sk.Version != 1 {
		t.Errorf("Version = %d, want 1", sk.Version)
	}
	if sk.ChainIndex != 0 {
		t.Errorf("ChainIndex = %d, want 0", sk.ChainIndex)
	}
	if len(sk.ChainKey) != ChainKeySize {
		t.Errorf("ChainKey size = %d, want %d", len(sk.ChainKey), ChainKeySize)
	}
	if len(sk.Signature) == 0 {
		t.Error("Signature missing")
	}
}

func TestSenderKeyVerifyRoundtrip(t *testing.T) {
	sk, priv, _ := makeSenderKey(t)
	if err := sk.Verify(pub(priv)); err != nil {
		t.Errorf("Verify after construction: %v", err)
	}
}

func TestSenderKeyVerifyWrongPubFails(t *testing.T) {
	sk, _, _ := makeSenderKey(t)
	wrong, _ := crypto.GenerateSM2Key()
	if err := sk.Verify(pub(wrong)); err == nil {
		t.Error("Verify accepted wrong pub")
	}
}

func TestSenderKeyVerifyTamperedFails(t *testing.T) {
	sk, priv, _ := makeSenderKey(t)
	// Tamper with ChainKey.
	sk.ChainKey[0] ^= 0xFF
	if err := sk.Verify(pub(priv)); err == nil {
		t.Error("Verify accepted tampered ChainKey")
	}
}

func TestSenderKeyAdvanceRatchets(t *testing.T) {
	sk, priv, _ := makeSenderKey(t)
	oldKey := append([]byte(nil), sk.ChainKey...)
	oldIndex := sk.ChainIndex

	next, err := Advance(sk, priv)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	// Chain key must change.
	if bytes.Equal(oldKey, next.ChainKey) {
		t.Error("Advance did not change ChainKey")
	}
	// Index must increment.
	if next.ChainIndex != oldIndex+1 {
		t.Errorf("ChainIndex = %d, want %d", next.ChainIndex, oldIndex+1)
	}
	// Original SenderKey must be untouched (Advance is a pure
	// function — caller decides whether to overwrite).
	if !bytes.Equal(sk.ChainKey, oldKey) {
		t.Error("Advance mutated original ChainKey")
	}
	if sk.ChainIndex != oldIndex {
		t.Error("Advance mutated original ChainIndex")
	}
	// Next must Verify under the same priv.
	if err := next.Verify(pub(priv)); err != nil {
		t.Errorf("Verify next: %v", err)
	}
}

func TestSenderKeyAdvanceDeterministic(t *testing.T) {
	// Two SenderKeys with the same starting ChainKey advance to
	// the same next ChainKey. This is a sanity check that the
	// ratchet isn't accidentally random.
	sk1, priv1, _ := makeSenderKey(t)
	sk2, priv2, _ := makeSenderKey(t)
	// Force them to the same ChainKey + ChainIndex + IssuedAt.
	sk2.ChainKey = append([]byte(nil), sk1.ChainKey...)
	sk2.ChainIndex = sk1.ChainIndex
	sk2.IssuedAt = sk1.IssuedAt
	// Resign sk2 with priv2 so Verify works on the next.
	if err := resign(sk2, priv2); err != nil {
		t.Fatal(err)
	}
	next1, err := Advance(sk1, priv1)
	if err != nil {
		t.Fatal(err)
	}
	next2, err := Advance(sk2, priv2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(next1.ChainKey, next2.ChainKey) {
		t.Errorf("non-deterministic ratchet: %x vs %x", next1.ChainKey, next2.ChainKey)
	}
}

func TestSenderKeyAdvanceManyTimes(t *testing.T) {
	// 1000 ratchets — make sure no off-by-one or wraparound in
	// the chain index.
	sk, priv, _ := makeSenderKey(t)
	for i := 0; i < 1000; i++ {
		next, err := Advance(sk, priv)
		if err != nil {
			t.Fatalf("Advance at %d: %v", i, err)
		}
		if next.ChainIndex != ChainIndex(i+1) {
			t.Errorf("ChainIndex at step %d = %d, want %d", i, next.ChainIndex, i+1)
		}
		sk = next
	}
}

// resign re-signs a SenderKey with the given priv (used in
// deterministic-advance tests where we hand-craft a SenderKey
// that needs a fresh signature).
func resign(sk *SenderKey, priv *crypto.SM2PrivateKey) error {
	sig, err := crypto.SignSM2(priv, sk.toCanonical())
	if err != nil {
		return err
	}
	sk.Signature = sig
	return nil
}
