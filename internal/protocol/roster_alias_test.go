package protocol

import (
	"encoding/json"
	"testing"
	"time"
)

// TestRosterEntryAliasRoundTrip: the alias field is
// carried on the wire when non-empty (omitted when
// empty via `omitempty`). Older receivers that don't
// know the field simply ignore it (json default).
func TestRosterEntryAliasRoundTrip(t *testing.T) {
	with := RosterEntry{
		PeerID:    "0123456789abcdef0123456789abcdef",
		Hostname:  "alice-laptop",
		Alias:     "老板",
		Addrs:     []string{"192.168.40.5:4748"},
		FirstSeen: time.Unix(1700000000, 0).UTC(),
	}
	data, err := json.Marshal(with)
	if err != nil {
		t.Fatal(err)
	}
	// The alias must appear in the JSON output.
	if !bytesContain(data, []byte(`"alias":"老板"`)) {
		t.Errorf("alias not in wire payload: %s", data)
	}

	var got RosterEntry
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Alias != "老板" {
		t.Errorf("Alias round-trip = %q, want 老板", got.Alias)
	}
}

// TestRosterEntryAliasOmittedWhenEmpty: alias is
// `omitempty` — an empty string drops off the wire
// entirely. This is what we want for both backward
// compat (older nodes won't see the field at all and
// will default to "") AND forward compat (a node that
// DOESN'T set alias doesn't pay any wire cost).
func TestRosterEntryAliasOmittedWhenEmpty(t *testing.T) {
	empty := RosterEntry{
		PeerID:   "0123456789abcdef0123456789abcdef",
		Hostname: "alice",
		Addrs:    []string{"192.168.40.5:4748"},
	}
	data, err := json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	if bytesContain(data, []byte(`"alias"`)) {
		t.Errorf("alias key in payload despite empty: %s", data)
	}
}

// TestRosterEntryBackwardCompat: a v1 RosterEntry JSON
// without `alias` deserializes cleanly with Alias="".
// This is the forward-compat path: a new sender can
// send alias, an old receiver ignores it. The
// reverse — old sender omits alias, new receiver
// defaults to "" — is what this test verifies.
func TestRosterEntryBackwardCompat(t *testing.T) {
	const v1 = `{
  "peer_id": "0123456789abcdef0123456789abcdef",
  "hostname": "alice",
  "addrs": ["192.168.40.5:4748"],
  "first_seen": "2025-12-31T23:59:59Z"
}`
	var got RosterEntry
	if err := json.Unmarshal([]byte(v1), &got); err != nil {
		t.Fatal(err)
	}
	if got.Alias != "" {
		t.Errorf("legacy peerID parsed with Alias = %q, want empty", got.Alias)
	}
	if got.Hostname != "alice" {
		t.Errorf("legacy Hostname = %q, want alice", got.Hostname)
	}
}

// bytesContain is a tiny helper to keep the test
// self-contained without importing bytes just for
// Contains on a []byte.
func bytesContain(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	if len(haystack) < len(needle) {
		return false
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}