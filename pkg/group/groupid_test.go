package group

import (
	"bytes"
	"encoding/hex"
	"testing"
	"time"
)

// Compile-time check: RenderedGroupIDSize must equal what
// hex.EncodedLen(RawGroupIDSize) + 2 evaluates to. If we ever
// change RawGroupIDSize, this test will catch the mismatch
// before a runtime panic.
func TestRenderedGroupIDSizeMatchesHex(t *testing.T) {
	want := 2 + hex.EncodedLen(RawGroupIDSize)
	if RenderedGroupIDSize != want {
		t.Errorf("RenderedGroupIDSize = %d, want %d (2+hex.EncodedLen(%d))",
			RenderedGroupIDSize, want, RawGroupIDSize)
	}
}

func TestComputeGroupIDDeterministic(t *testing.T) {
	// Same inputs → same GroupID. Critical property: a peer who
	// independently observes the create parameters can reproduce
	// the ID without contacting anyone.
	creator := bytes.Repeat([]byte{0xAB}, 16)
	t0 := time.Date(2026, 6, 27, 22, 0, 0, 0, time.UTC)
	gid1 := ComputeGroupID(creator, "project team", t0)
	gid2 := ComputeGroupID(creator, "project team", t0)
	if !bytes.Equal(gid1, gid2) {
		t.Errorf("ComputeGroupID not deterministic: %x vs %x", gid1, gid2)
	}
	if len(gid1) != RawGroupIDSize {
		t.Errorf("ComputeGroupID size = %d, want %d", len(gid1), RawGroupIDSize)
	}
}

func TestComputeGroupIDInputsAffectOutput(t *testing.T) {
	// Any input change → different ID. This is what makes the
	// ID content-addressed.
	creator := bytes.Repeat([]byte{0xAB}, 16)
	t0 := time.Date(2026, 6, 27, 22, 0, 0, 0, time.UTC)

	base := ComputeGroupID(creator, "project team", t0)

	// Different creator
	if bytes.Equal(base, ComputeGroupID(bytes.Repeat([]byte{0xCD}, 16), "project team", t0)) {
		t.Error("different creator produced same ID")
	}
	// Different name
	if bytes.Equal(base, ComputeGroupID(creator, "different name", t0)) {
		t.Error("different name produced same ID")
	}
	// Different timestamp (1ns later)
	if bytes.Equal(base, ComputeGroupID(creator, "project team", t0.Add(time.Nanosecond))) {
		t.Error("different timestamp produced same ID")
	}
}

func TestRenderAndParseRoundtrip(t *testing.T) {
	creator := bytes.Repeat([]byte{0x42}, 16)
	gid := ComputeGroupID(creator, "test", time.Now())
	rendered := RenderGroupID(gid)
	if len(rendered) != RenderedGroupIDSize {
		t.Errorf("Rendered length = %d, want %d", len(rendered), RenderedGroupIDSize)
	}
	if rendered[0] != 'g' {
		t.Errorf("Rendered prefix = %q, want 'g'", rendered[0])
	}
	parsed, err := ParseGroupID(rendered)
	if err != nil {
		t.Fatalf("ParseGroupID: %v", err)
	}
	if !bytes.Equal(parsed, gid) {
		t.Errorf("Parse mismatch: got %x, want %x", parsed, gid)
	}
}

func TestParseAcceptsBareHex(t *testing.T) {
	gid := ComputeGroupID([]byte("creator"), "x", time.Now())
	bare := hex.EncodeToString(gid)
	parsed, err := ParseGroupID(bare)
	if err != nil {
		t.Fatalf("ParseGroupID bare: %v", err)
	}
	if !bytes.Equal(parsed, gid) {
		t.Errorf("bare hex parse mismatch")
	}
}

func TestParseRejectsBadInputs(t *testing.T) {
	for _, bad := range []string{
		"",                       // empty
		"g_short",                // too short with prefix
		"g_" + hex.EncodeToString([]byte{1, 2, 3}), // wrong size with prefix
		hex.EncodeToString([]byte{1, 2, 3}),         // wrong size bare
		"g_" + hex.EncodeToString(make([]byte, RawGroupIDSize)) + "extra", // too long
		"x_" + hex.EncodeToString(make([]byte, RawGroupIDSize)), // wrong prefix
	} {
		if _, err := ParseGroupID(bad); err == nil {
			t.Errorf("ParseGroupID(%q) accepted bad input", bad)
		}
	}
}
