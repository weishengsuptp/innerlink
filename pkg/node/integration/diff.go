// State-diff engine for the integration harness. After each
// action (or after a "settle" delay) the test calls
// h.Snapshot() and feeds it into Diff() or AssertConsistent()
// to detect cross-peer divergence — the exact class of bug
// we've been hitting all week (19:48 leave-not-replay,
// 21:08 re-accept-leavelog, 21:20 hostname-dup + member-desync,
// 21:43 ListGroups race).
//
// Diff returns a list of human-readable strings, one per
// divergence. AssertConsistent fails the test if any
// divergence is found. AssertGroupMemberSet asserts on
// ONE specific group: the most common check after a
// scenario action.

package integration_test

import (
	"fmt"
	"sort"
	"strings"

	"github.com/weishengsuptp/innerlink/pkg/node"
)

// Diff returns every cross-peer divergence in snap.
// Each entry is a single sentence starting with "[A|B]
// saw X, [C] saw Y". Empty slice means consistent.
//
// The set of invariants we currently check:
//   1. For every group that exists in any peer's
//      GroupDirs, every peer's GroupDirs for that group
//      must agree on the member set.
//   2. For every peerID that any peer lists in their
//      RosterByID, every other peer that has that peer
//      in their RosterByID must agree on the Reset flag
//      (alias may legitimately diverge — discovery
//      races — so we don't pin that).
//   3. Leavelog contents are private to each peer
//      (it's the leaver's "I left this" record); we
//      don't cross-peer check it. But within one peer's
//      view, every groupID they have in GroupDirs
//      should NOT be in their own leavelog (otherwise
//      the "ApplyRosterUpdate skip-if-in-leavelog" guard
//      would suppress their re-add — see 21:08 bug).
func Diff(snap *Snapshot) []string {
	if snap == nil {
		return []string{"nil snapshot"}
	}
	var diffs []string

	// Gather per-peer group IDs.
	allGroups := map[string]bool{}
	for _, ps := range snap.PerPeer {
		for gid := range ps.GroupDirs {
			allGroups[gid] = true
		}
	}
	// For each group, compare member sets across peers.
	for gid := range allGroups {
		// Skip groups that no peer has yet (e.g. one
		// peer left and a tombstoned record remains
		// transiently).
		var sawAtLeastOne bool
		for _, ps := range snap.PerPeer {
			if _, ok := ps.GroupDirs[gid]; ok {
				sawAtLeastOne = true
				break
			}
		}
		if !sawAtLeastOne {
			continue
		}
		perPeerMembers := map[string][]string{}
		for name, ps := range snap.PerPeer {
			if mems, ok := ps.GroupDirs[gid]; ok {
				// Sort for stable diff output.
				sorted := append([]string(nil), mems...)
				sort.Strings(sorted)
				perPeerMembers[name] = sorted
			}
		}
		if len(perPeerMembers) < 2 {
			continue
		}
		// Find the "canonical" view as the one with the
		// most members (creation-authoritative). For
		// our scenarios, the creator's view has the
		// most because they were the first to know
		// about every accept. If two peer views
		// disagree, the one missing members is the
		// stale one.
		var canonical []string
		var canonicalPeer string
		for name, mems := range perPeerMembers {
			if len(mems) > len(canonical) {
				canonical = mems
				canonicalPeer = name
			}
		}
		for name, mems := range perPeerMembers {
			if !sameStringSet(mems, canonical) {
				diffs = append(diffs, fmt.Sprintf(
					"group %s member mismatch: %s saw %v, %s (canonical) saw %v",
					shortGroupID(gid), name, mems, canonicalPeer, canonical))
			}
		}
	}

	// For every peer that has a roster entry, every
	// OTHER peer that also lists that peerID must
	// agree on Reset. Alias divergence is tolerated.
	allPeers := map[string]bool{}
	for _, ps := range snap.PerPeer {
		for pid := range ps.RosterByID {
			allPeers[pid] = true
		}
	}
	for pid := range allPeers {
		var resetByPeer = map[string]bool{}
		var sawAtLeastOne bool
		for name, ps := range snap.PerPeer {
			if entry, ok := ps.RosterByID[pid]; ok {
				resetByPeer[name] = entry.Reset
				sawAtLeastOne = true
			}
		}
		if !sawAtLeastOne || len(resetByPeer) < 2 {
			continue
		}
		// All resets must agree. Reset means "this
		// peerID was newly seen after a wipe event" — if
		// one peer thinks so and another doesn't, the
		// wipe-gossip path is broken (only the first
		// peer to observe the wipe propagates it).
		if !consistent(resetByPeer) {
			parts := []string{}
			for name, v := range resetByPeer {
				parts = append(parts, fmt.Sprintf("%s=%v", name, v))
			}
			sort.Strings(parts)
			diffs = append(diffs, fmt.Sprintf(
				"peer %s Reset flag disagrees across peers: %s",
				shortPeerID(pid), strings.Join(parts, " ")))
		}
	}

	// Per-peer: every group ID in their GroupDirs must
	// NOT be in their own leavelog. This is the
	// v1.1.4 followup invariant: re-accept must
	// clear the leavelog entry, otherwise
	// ApplyRosterUpdate keeps the peer at "1 member"
	// instead of catching up to the creator's roster.
	for name, ps := range snap.PerPeer {
		leavelogSet := map[string]bool{}
		for _, le := range ps.Leavelog {
			leavelogSet[le.GroupID] = true
		}
		for gid := range ps.GroupDirs {
			if leavelogSet[gid] {
				diffs = append(diffs, fmt.Sprintf(
					"peer %s has group %s in BOTH GroupDirs and Leavelog (re-accept did not clear leavelog)",
					name, shortGroupID(gid)))
			}
		}
	}

	return diffs
}

// AssertConsistent fails the test if Diff(snap) returns
// any divergences. The error log includes the diff so the
// reader sees exactly what each peer saw.
func AssertConsistent(t T, snap *Snapshot) {
	t.Helper()
	diffs := Diff(snap)
	if len(diffs) == 0 {
		return
	}
	t.Errorf("inconsistent state across peers (%d divergence%s):",
		len(diffs), plural(len(diffs)))
	for _, d := range diffs {
		t.Errorf("  %s", d)
	}
}

// AssertGroupMemberSet asserts that for a single group, the
// set of member peerIDs on `peerName`'s view exactly
// matches `wantMembers`. Empty wantMembers is an error (use
// HasGroup= false via AssertGroupAbsent if you actually
// want an empty group).
//
// wantMembers can be either friendly peer names ("alice")
// or hex peer IDs — both forms are accepted. The assertion
// resolves names against snap.PerPeer[*].PeerID() before
// comparison.
func AssertGroupMemberSet(t T, snap *Snapshot, peerName, groupID string, wantMembers ...string) {
	t.Helper()
	ps, ok := snap.PerPeer[peerName]
	if !ok {
		t.Fatalf("AssertGroupMemberSet: no peer %q in snapshot", peerName)
	}
	got, ok := ps.GroupDirs[groupID]
	if !ok {
		t.Errorf("AssertGroupMemberSet: peer %s does NOT have group %s (snapshot knows %v)",
			peerName, shortGroupID(groupID), peerGroupList(ps))
		return
	}
	gotSet := map[string]bool{}
	for _, p := range got {
		gotSet[p] = true
	}
	wantSet := map[string]bool{}
	for _, p := range wantMembers {
		resolved := resolveName(snap, p)
		wantSet[resolved] = true
	}
	for p := range wantSet {
		if !gotSet[p] {
			t.Errorf("AssertGroupMemberSet: peer %s group %s MISSING member %s",
				peerName, shortGroupID(groupID), shortPeerID(p))
		}
	}
	for p := range gotSet {
		if !wantSet[p] {
			t.Errorf("AssertGroupMemberSet: peer %s group %s UNEXPECTED extra member %s",
				peerName, shortGroupID(groupID), shortPeerID(p))
		}
	}
}

// resolveName returns the hex peer ID for a friendly peer
// name in the snapshot, or the input unchanged if it
// doesn't match any known peer (allowing hex IDs to pass
// through).
func resolveName(snap *Snapshot, nameOrHex string) string {
	if snap == nil {
		return nameOrHex
	}
	// Hex peer IDs are 32 lowercase hex chars; if the
	// input matches that shape, it's already an ID.
	if len(nameOrHex) == 32 && isLowerHex(nameOrHex) {
		return nameOrHex
	}
	if ps, ok := snap.PerPeer[nameOrHex]; ok {
		return ps.PeerID
	}
	return nameOrHex
}

func isLowerHex(s string) bool {
	for _, c := range s {
		switch {
		case '0' <= c && c <= '9':
		case 'a' <= c && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// AssertGroupAbsent asserts that peerName does NOT have
// groupID in its GroupDirs. Used when we expect a group
// to be dissolved after a LeaveGroup-by-non-creator +
// roster-update chain.
func AssertGroupAbsent(t T, snap *Snapshot, peerName, groupID string) {
	t.Helper()
	ps, ok := snap.PerPeer[peerName]
	if !ok {
		t.Fatalf("AssertGroupAbsent: no peer %q in snapshot", peerName)
	}
	if _, ok := ps.GroupDirs[groupID]; ok {
		t.Errorf("AssertGroupAbsent: peer %s STILL has group %s (expected gone)",
			peerName, shortGroupID(groupID))
	}
}

// AssertPeerInLeavelog asserts that peerName has
// groupID in their Leavelog (i.e., they recorded a leave).
func AssertPeerInLeavelog(t T, snap *Snapshot, peerName, groupID string, want bool) {
	t.Helper()
	ps, ok := snap.PerPeer[peerName]
	if !ok {
		t.Fatalf("AssertPeerInLeavelog: no peer %q in snapshot", peerName)
	}
	for _, le := range ps.Leavelog {
		if le.GroupID == groupID {
			if !want {
				t.Errorf("AssertPeerInLeavelog: peer %s has %s in leavelog (expected absent)",
					peerName, shortGroupID(groupID))
			}
			return
		}
	}
	if want {
		t.Errorf("AssertPeerInLeavelog: peer %s does NOT have %s in leavelog (expected present)",
			peerName, shortGroupID(groupID))
	}
}

// T is the minimal interface the diff engine needs from
// the testing framework. Both *testing.T and a fake
// helper implement this; using an interface keeps Diff
// / AssertXxx free of testing-framework imports for
// other callers.
type T interface {
	Helper()
	Errorf(format string, args ...interface{})
	Fatalf(format string, args ...interface{})
}

// --- internal helpers ---

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	am := map[string]int{}
	for _, s := range a {
		am[s]++
	}
	for _, s := range b {
		am[s]--
		if am[s] < 0 {
			return false
		}
	}
	return true
}

// consistent reports whether every value in m is equal.
func consistent(m map[string]bool) bool {
	var first bool
	var v bool
	for _, x := range m {
		if !first {
			first = true
			v = x
			continue
		}
		if x != v {
			return false
		}
	}
	return true
}

// shortGroupID / shortPeerID shrink long hex strings for
// readable error messages.
func shortGroupID(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:8] + "…" + s[len(s)-4:]
}
func shortPeerID(s string) string {
	if len(s) <= 10 {
		return s
	}
	return s[:4] + "…" + s[len(s)-4:]
}

func peerGroupList(ps *PeerSnapshot) []string {
	out := []string{}
	for gid := range ps.GroupDirs {
		out = append(out, gid)
	}
	sort.Strings(out)
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// Compile-time sanity check that node.GroupInfo still
// exists (the diff engine imports it only via the
// snapshot; this protects against silent rename).
var _ = node.GroupInfo{}
