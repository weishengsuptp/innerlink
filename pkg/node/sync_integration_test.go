package node

// sync_integration_test.go — node-level tests for v1.1.4
// (2026-07-02) self-claim + audit infrastructure.
//
// Strategy: stand up a Node A in a temp dir, do the
// bookkeeping that mimics a wipe+reinstall (write a
// self_history.json with an "old" peerID, manually
// add the same old peerID to roster + groups + aliases),
// then stand up Node B in the SAME data dir (simulating
// the same device re-launching with a new device.key)
// and assert that startup sync migrated everything.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/weishengsuptp/innerlink/internal/alias"
	"github.com/weishengsuptp/innerlink/internal/roster"
	"github.com/weishengsuptp/innerlink/internal/selfid"
	"github.com/weishengsuptp/innerlink/pkg/group"
)

// freshDataDir creates a clean per-test directory with all
// the files a Node expects. The Node.New call will then
// load the existing selfid history (so we can simulate
// a wipe+reinstall by writing a history entry with a
// prior peerID before New is called).
func freshDataDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	for _, sub := range []string{".innerlink", "received"} {
		if err := os.MkdirAll(filepath.Join(d, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// seedSelfHistory writes a self_history.json with a single
// entry pointing at oldPeerID as the most recent NewPeerID.
// This is the "wipe happened" state — the next launch will
// see a different device.key and record a wipe+reinstall
// migration in New().
//
func seedSelfHistory(t *testing.T, dataDir, oldPeerID string) {
	t.Helper()
	store, err := selfid.Open(filepath.Join(dataDir, "self_history.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordMigration(selfid.Entry{
		NewPeerID:  oldPeerID,
		SwitchedAt: time.Now().UTC().Add(-1 * time.Hour),
		Trigger:    selfid.TriggerFreshInstall,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
}

// seedRoster adds the old peerID as a "live" entry
// (simulating that some peer still has it on disk and
// the wipe+reinstall peer's dedup hasn't kicked in yet).
func seedRoster(t *testing.T, dataDir, oldPeerID string) {
	t.Helper()
	rs, err := roster.Open(filepath.Join(dataDir, ".innerlink", "roster.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rs.Add(roster.Entry{
		PeerID:    oldPeerID,
		Hostname:  "<old-host>",
		Alias:     "old-me",
		Addrs:     []string{"192.168.40.128:4748"},
		FirstSeen: time.Now().UTC().Add(-24 * time.Hour),
		LastSeen:  time.Now().UTC().Add(-1 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := rs.Save(); err != nil {
		t.Fatal(err)
	}
}

// seedAlias adds an alias for the old peerID. Claim
// should rekey this to the new peerID.
func seedAlias(t *testing.T, dataDir, oldPeerID string) {
	t.Helper()
	as, err := alias.Open(filepath.Join(dataDir, ".innerlink", "aliases.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := as.Set(oldPeerID, "old-self-alias"); err != nil {
		t.Fatal(err)
	}
	if err := as.Save(); err != nil {
		t.Fatal(err)
	}
}

// seedGroup creates a group where the creator is the OLD
// peerID, with two members (old-peerID as creator + a fake
// "other peer" that's NOT the old peerID — to make sure
// claim only touches the old peerID's records).
func seedGroup(t *testing.T, dataDir, oldPeerID, fakeOtherPeer string) string {
	t.Helper()
	rawID := group.ComputeGroupID([]byte("seed"), "test-group", time.Now().UTC())
	rendered := group.RenderGroupID(rawID)
	m := &group.Members{
		GroupID:   rendered,
		GroupName: "test-group",
		Creator:   oldPeerID,
		CreatedAt: time.Now().UTC(),
		Members: []group.Member{
			{PeerID: oldPeerID, JoinedAt: time.Now().UTC(), IsCreator: true},
			{PeerID: fakeOtherPeer, JoinedAt: time.Now().UTC()},
		},
	}
	if err := m.Save(dataDir, rawID); err != nil {
		t.Fatal(err)
	}
	// Also seed the chat.enc side so ListGroups sees the
	// group. We write an empty file; the real path treats
	// an empty chat.enc as "no history yet" (readFrame loop
	// returns nothing, no error). storage.ListGroups only
	// requires the file to EXIST.
	chatDir := filepath.Join(dataDir, "chat", "groups", rendered)
	if err := os.MkdirAll(chatDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chatDir, "chat.enc"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return rendered
}

// TestSync_ClaimSelfIdentity covers the whole wipe+reinstall
// flow: pre-seeded self_history + roster + alias + group,
// then a Node.New with a fresh device.key, then
// runStartupSync. Asserts every place that referenced the
// old peerID now references the new one.
func TestSync_ClaimSelfIdentity(t *testing.T) {
	const oldPeerID = "0123456789abcdef0123456789abcdef"
	const fakeOtherPeer = "fedcba9876543210fedcba9876543210"

	d := freshDataDir(t)
	innerlinkDir := filepath.Join(d, ".innerlink")
	seedSelfHistory(t, innerlinkDir, oldPeerID)
	seedRoster(t, d, oldPeerID)
	seedAlias(t, d, oldPeerID)
	seedGroup(t, innerlinkDir, oldPeerID, fakeOtherPeer)

	// Stand up the Node. device.key doesn't exist yet, so
	// LoadOrCreate generates a fresh one. The New() path
	// sees a different peerID than the seeded selfid
	// Latest.NewPeerID and records a wipe+reinstall.
	logFile := filepath.Join(d, "test.log")
	n, err := New(Options{DataDir: filepath.Join(d, ".innerlink"), LogFile: logFile, LogLevel: "error"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = n.Close() })
	newPeerID := n.id.PeerIDHex()
	if newPeerID == oldPeerID {
		t.Fatalf("expected a fresh device.key, but got the same peerID %s", newPeerID)
	}

	// Manually run the claim (Start is skipped because we
	// don't want the dispatcher / TCP listener in this test).
	stats := n.claimSelfIdentity()
	if stats.membersReplaced == 0 {
		t.Errorf("membersReplaced = 0, want >=1 (creator + member)")
	}
	if stats.creatorsReplaced == 0 {
		t.Errorf("creatorsReplaced = 0, want 1")
	}
	if stats.aliasesRekeyed == 0 {
		t.Errorf("aliasesRekeyed = 0, want 1")
	}
	if !stats.rosterMarked {
		t.Errorf("rosterMarked = false, want true")
	}

	// Verify: roster entry for oldPeerID is reset=true.
	rosterEntry, err := n.rosterStore.Get(oldPeerID)
	if err != nil {
		t.Fatalf("roster.Get(%s): %v", oldPeerID[:8], err)
	}
	if !rosterEntry.Reset {
		t.Errorf("roster entry for %s has Reset=false after claim, want true", oldPeerID[:8])
	}

	// Verify: alias for oldPeerID is GONE; newPeerID has
	// the alias that was on oldPeerID.
	if _, ok := n.aliasStore.Get(oldPeerID); ok {
		t.Errorf("alias still keyed by old peerID %s after claim, want rekeyed", oldPeerID[:8])
	}
	gotAlias, ok := n.aliasStore.Get(newPeerID)
	if !ok {
		t.Errorf("alias missing for new peerID %s after claim, want rekeyed", newPeerID[:8])
	}
	if gotAlias.Name != "old-self-alias" {
		t.Errorf("alias.Name = %q, want %q (rekey should preserve the name)", gotAlias.Name, "old-self-alias")
	}

	// Verify: group members.json now references new peerID,
	// NOT the old one, and the fake other peer is untouched.
	renderedIDs, _ := n.chatStore.ListGroups()
	if len(renderedIDs) == 0 {
		t.Fatal("seeded group not visible to ListGroups")
	}
	for _, rendered := range renderedIDs {
		rawID, _ := group.ParseGroupID(rendered)
		m, err := group.LoadMembers(n.dataDir(), rawID)
		if err != nil {
			t.Fatalf("LoadMembers: %v", err)
		}
		if m.Creator == oldPeerID {
			t.Errorf("group %s Creator still %s after claim, want %s", rendered[:8], oldPeerID[:8], newPeerID[:8])
		}
		if m.Creator != newPeerID {
			t.Errorf("group %s Creator = %s, want %s", rendered[:8], m.Creator[:8], newPeerID[:8])
		}
		var sawOldMember, sawNewMember, sawFake bool
		for _, mem := range m.Members {
			if mem.PeerID == oldPeerID {
				sawOldMember = true
			}
			if mem.PeerID == newPeerID {
				sawNewMember = true
			}
			if mem.PeerID == fakeOtherPeer {
				sawFake = true
			}
		}
		if sawOldMember {
			t.Errorf("group %s still has member with old peerID %s", rendered[:8], oldPeerID[:8])
		}
		if !sawNewMember {
			t.Errorf("group %s missing member with new peerID %s", rendered[:8], newPeerID[:8])
		}
		if !sawFake {
			t.Errorf("group %s lost the unrelated fake peer %s (claim should be surgical)", rendered[:8], fakeOtherPeer[:8])
		}
	}
}

// TestSync_AuditRosterAndGroups_DropsTombstonedMembers:
// pre-seed a group with a tombstoned member, run audit,
// assert the member is gone.
func TestSync_AuditRosterAndGroups_DropsTombstonedMembers(t *testing.T) {
	const tombstoneID = "11112222333344445555666677778888"
	const liveID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa01"
	const liveID2 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa02"
	const selfHex = "ffffffffffffffffffffffffffffffff"

	d := freshDataDir(t)
	innerlinkDir := filepath.Join(d, ".innerlink")

	// Roster: tombstoneID is reset=true; liveID + liveID2
	// are active.
	rs, _ := roster.Open(filepath.Join(innerlinkDir, "roster.json"))
	now := time.Now().UTC()
	if _, err := rs.Add(roster.Entry{PeerID: tombstoneID, Hostname: "dead", Addrs: []string{"1.1.1.1:4748"}, FirstSeen: now, LastSeen: now, Reset: true}); err != nil {
		t.Fatal(err)
	}
	for i, pid := range []string{liveID, liveID2} {
		// Unique hostnames + unique IPs so Add()'s dedup
		// scan doesn't mark the first one as a tombstone
		// when the second is added. (The dedup matches on
		// hostname + IP overlap; if both matched, the
		// first entry gets reset=true on the second add,
		// which is the production-correct behaviour but
		// confuses this test.)
		if _, err := rs.Add(roster.Entry{
			PeerID:    pid,
			Hostname:  "alive-" + string(rune('a'+i)),
			Addrs:     []string{"2.2.2." + string(rune('1'+i)) + ":4748"},
			FirstSeen: now,
			LastSeen:  now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// self entry too, so audit doesn't trip on missing self.
	if _, err := rs.Add(roster.Entry{PeerID: selfHex, Hostname: "me", Addrs: []string{"3.3.3.3:4748"}, FirstSeen: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	if err := rs.Save(); err != nil {
		t.Fatal(err)
	}

	// Group: members include the tombstone + the two live + self.
	rawID := group.ComputeGroupID([]byte("seed"), "audit-group", now)
	rendered := group.RenderGroupID(rawID)
	m := &group.Members{
		GroupID:   rendered,
		GroupName: "audit-group",
		Creator:   selfHex,
		CreatedAt: now,
		Members: []group.Member{
			{PeerID: selfHex, JoinedAt: now, IsCreator: true},
			{PeerID: tombstoneID, JoinedAt: now},
			{PeerID: liveID, JoinedAt: now},
			{PeerID: liveID2, JoinedAt: now},
		},
	}
	if err := m.Save(innerlinkDir, rawID); err != nil {
		t.Fatal(err)
	}
	chatDir := filepath.Join(innerlinkDir, "chat", "groups", rendered)
	if err := os.MkdirAll(chatDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chatDir, "chat.enc"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// Boot a Node.
	logFile := filepath.Join(d, "test.log")
	n, err := New(Options{DataDir: innerlinkDir, LogFile: logFile, LogLevel: "error"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Close() })

	stats := n.auditRosterAndGroups()
	if stats.groupMembersRemoved != 1 {
		t.Errorf("groupMembersRemoved = %d, want 1 (the tombstone)", stats.groupMembersRemoved)
	}

	// Verify the group now has self + 2 live, no tombstone.
	m2, err := group.LoadMembers(n.dataDir(), rawID)
	if err != nil {
		t.Fatal(err)
	}
	if len(m2.Members) != 3 {
		t.Errorf("post-audit group has %d members, want 3 (self + 2 live)", len(m2.Members))
	}
	for _, mem := range m2.Members {
		if mem.PeerID == tombstoneID {
			t.Error("tombstone still in group after audit")
		}
	}
	// Creator should be unchanged (self was preserved, not removed).
	if m2.Creator != selfHex {
		t.Errorf("Creator = %s, want self %s", m2.Creator[:8], selfHex[:8])
	}
}

// TestSync_AuditRoster_DropsStaleTombstones: pre-seed a
// tombstone older than the cutoff, run audit, assert it
// got dropped from the in-memory map AND the on-disk file.
func TestSync_AuditRoster_DropsStaleTombstones(t *testing.T) {
	d := freshDataDir(t)
	rs, _ := roster.Open(filepath.Join(d, ".innerlink", "roster.json"))
	now := time.Now().UTC()
	// 48h-old tombstone: must be dropped (cutoff is 24h).
	if _, err := rs.Add(roster.Entry{
		PeerID:    "99999999999999999999999999999991",
		Hostname:  "stale",
		Addrs:     []string{"5.5.5.5:4748"},
		FirstSeen: now.Add(-72 * time.Hour),
		LastSeen:  now.Add(-48 * time.Hour),
		Reset:     true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := rs.Save(); err != nil {
		t.Fatal(err)
	}

	logFile := filepath.Join(d, "test.log")
	n, err := New(Options{DataDir: filepath.Join(d, ".innerlink"), LogFile: logFile, LogLevel: "error"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Close() })

	stats := n.auditRosterAndGroups()
	if stats.rosterTombstonesDropped != 1 {
		t.Errorf("rosterTombstonesDropped = %d, want 1", stats.rosterTombstonesDropped)
	}

	// Reload from disk; the tombstone should be gone.
	rs2, _ := roster.Open(filepath.Join(d, ".innerlink", "roster.json"))
	if _, err := rs2.Get("99999999999999999999999999999991"); err == nil {
		t.Error("stale tombstone survived audit + Save, want dropped on disk")
	}
}

// TestSync_ClaimIsNoOpForFreshInstall: a Node that sees
// an empty self_history (or one whose Latest.NewPeerID
// matches its own) must not touch any state. Belt-and-
// suspenders: claim returns 0 stats, no log lines
// suggesting anything happened.
func TestSync_ClaimIsNoOpForFreshInstall(t *testing.T) {
	d := freshDataDir(t)
	// No seedSelfHistory — fresh install, no prior identity.
	logFile := filepath.Join(d, "test.log")
	n, err := New(Options{DataDir: filepath.Join(d, ".innerlink"), LogFile: logFile, LogLevel: "error"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Close() })

	stats := n.claimSelfIdentity()
	if stats.membersReplaced != 0 || stats.creatorsReplaced != 0 ||
		stats.aliasesRekeyed != 0 || stats.rosterMarked {
		t.Errorf("claim on fresh install should be a no-op, got %+v", stats)
	}
}

// TestSync_SelfHistoryFileWrittenOnNew: confirm the
// first launch writes a self_history.json with one
// fresh_install entry, and the file is valid JSON that
// can be reloaded.
func TestSync_SelfHistoryFileWrittenOnNew(t *testing.T) {
	d := freshDataDir(t)
	logFile := filepath.Join(d, "test.log")
	n, err := New(Options{DataDir: filepath.Join(d, ".innerlink"), LogFile: logFile, LogLevel: "error"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = n.Close() })

	// Close to flush.
	if err := n.selfidStore.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(d, ".innerlink", "self_history.json"))
	if err != nil {
		t.Fatalf("self_history.json not on disk after New: %v", err)
	}
	var f struct {
		V       int              `json:"v"`
		Entries []selfid.Entry   `json:"entries"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("self_history.json not valid JSON: %v", err)
	}
	if len(f.Entries) != 1 {
		t.Errorf("entries len = %d, want 1", len(f.Entries))
	}
	if f.Entries[0].Trigger != selfid.TriggerFreshInstall {
		t.Errorf("entries[0].Trigger = %q, want fresh_install", f.Entries[0].Trigger)
	}
	if f.Entries[0].NewPeerID != n.id.PeerIDHex() {
		t.Errorf("entries[0].NewPeerID mismatch")
	}
}
