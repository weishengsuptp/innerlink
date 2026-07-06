// Scenario tests for multi-peer sync. These replay the
// bugs 潇男 reported on 2026-07-02 (v1.1.4 manual testing
// session) and a few synthetic stress cases.
//
// Each scenario is short and reads like a timeline of
// human-meaningful events. The harness + diff engine do
// the heavy lifting; the scenario's job is to assert
// the post-condition.
//
// Conventions:
//   - Peer names: "alice" (creator), "bob" / "carol"
//     (invitees). The 19:48/21:08/21:20 bugs all involved
//     a 3-person topology.
//   - Group names: short ASCII (e.g. "g1"). The exact
//     name doesn't matter; what's tested is membership
//     and message propagation.
//   - Each scenario ends with AssertConsistent + an
//     explicit AssertGroupMemberSet so a failure points
//     to the specific divergence.

package integration_test

import (
	"testing"
)

// TestScenario_ReAcceptAfterLeave — the 21:08 bug.
//
// Timeline:
//  1. alice creates g1, invites bob + carol.
//  2. bob and carol accept. All 3 see {alice, bob, carol}.
//  3. bob leaves. alice + carol should see {alice, carol};
//     bob should NOT see the group anymore.
//  4. alice re-invites bob. bob accepts again.
//  5. EXPECTED: all 3 see {alice, bob, carol}.
//
// What was broken before v1.1.4:
//   - After bob re-accepted, bob's local view was
//     still {bob} (the AcceptGroupInvite wrote members
//     = [bob] because his old leavelog entry was
//     suppressing roster updates).
//   - v1.1.4 fix: leavelog.Remove on accept + rookout
//     if-in-leavelog guard in ApplyRosterUpdate.
//
// What this test catches:
//   - Regression of the leavelog.Remove call.
//   - Failure to broadcast roster back to other members
//     after creator catches up (only creator sees
//     the new roster; bob + carol stay stuck).
//   - Skipping step 5's broadcast entirely.
func TestScenario_ReAcceptAfterLeave(t *testing.T) {
	h := NewHarness(t, []string{"alice", "bob", "carol"})

	// Step 1: alice creates g1.
	bobID, _ := h.ResolvePeerID("bob")
	carolID, _ := h.ResolvePeerID("carol")
	gid, err := h.CreateGroupAction("alice", "g1", []string{bobID, carolID})
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	// Step 2: bob + carol accept.
	invBob, err := h.InviteAction("alice", gid, bobID)
	if err != nil {
		t.Fatalf("Invite bob: %v", err)
	}
	if err := h.AcceptInviteAction("bob", invBob, "alice"); err != nil {
		t.Fatalf("Accept bob: %v", err)
	}

	invCarol, err := h.InviteAction("alice", gid, carolID)
	if err != nil {
		t.Fatalf("Invite carol: %v", err)
	}
	if err := h.AcceptInviteAction("carol", invCarol, "alice"); err != nil {
		t.Fatalf("Accept carol: %v", err)
	}

	// After both accept, everyone should see 3 members.
	snap := h.Snapshot()
	AssertConsistent(t, snap)
	AssertGroupMemberSet(t, snap, "alice", gid, "alice", "bob", "carol")
	AssertGroupMemberSet(t, snap, "bob", gid, "alice", "bob", "carol")
	AssertGroupMemberSet(t, snap, "carol", gid, "alice", "bob", "carol")

	// Step 3: bob leaves.
	if err := h.LeaveGroupAction("bob", gid); err != nil {
		t.Fatalf("bob LeaveGroup: %v", err)
	}
	snap = h.Snapshot()
	AssertConsistent(t, snap)
	AssertGroupMemberSet(t, snap, "alice", gid, "alice", "carol")
	AssertGroupMemberSet(t, snap, "carol", gid, "alice", "carol")
	AssertGroupAbsent(t, snap, "bob", gid)
	AssertPeerInLeavelog(t, snap, "bob", gid, true)

	// Step 4: alice re-invites bob, bob accepts.
	invBob2, err := h.InviteAction("alice", gid, bobID)
	if err != nil {
		t.Fatalf("Re-invite bob: %v", err)
	}
	if err := h.AcceptInviteAction("bob", invBob2, "alice"); err != nil {
		t.Fatalf("Re-accept bob: %v", err)
	}

	// Step 5: all 3 should see {alice, bob, carol}.
	// This is the assertion that catches the 21:08 bug:
	// without the leavelog.Remove + broadcast-roster fix,
	// bob stays at {bob} because his re-accept write gets
	// shadowed by his prior leavelog entry.
	snap = h.Snapshot()
	AssertConsistent(t, snap)
	AssertGroupMemberSet(t, snap, "alice", gid, "alice", "bob", "carol")
	AssertGroupMemberSet(t, snap, "bob", gid, "alice", "bob", "carol")
	AssertGroupMemberSet(t, snap, "carol", gid, "alice", "bob", "carol")
	// Bob's leavelog should be cleared on re-accept.
	AssertPeerInLeavelog(t, snap, "bob", gid, false)
}

// TestScenario_AcceptWhileOffline — the 19:48 bug.
//
// Timeline:
//  1. alice creates g1, invites bob + carol.
//  2. bob + carol accept. All 3 see {alice, bob, carol}.
//  3. alice goes "offline" (we model by suspending
//     pushes to her; carol + bob are still online).
//  4. bob leaves. LeaveNotice fanout reaches carol but
//     NOT alice (offline).
//  5. alice reconnects (resume pushes).
//  6. carol's broadcast of the post-leave roster
//     reaches alice on reconnect.
//
// EXPECTED after reconnect: alice sees {alice, carol}.
// Before v1.1.4: alice stayed at {alice, bob, carol}
// because nothing told her bob had left.
//
// We use alice as the offline peer here because the
// 19:48 user-reported bug was specifically: the
// CREATOR (alice) goes offline, a member leaves, the
// creator reconnects and doesn't learn about the leave
// until the next gossip tick.
func TestScenario_AcceptWhileOffline(t *testing.T) {
	h := NewHarness(t, []string{"alice", "bob", "carol"})

	bobID, _ := h.ResolvePeerID("bob")
	carolID, _ := h.ResolvePeerID("carol")

	gid, err := h.CreateGroupAction("alice", "g1", []string{bobID, carolID})
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	// Step 2: bob + carol accept.
	invBob, err := h.InviteAction("alice", gid, bobID)
	if err != nil {
		t.Fatalf("Invite bob: %v", err)
	}
	if err := h.AcceptInviteAction("bob", invBob, "alice"); err != nil {
		t.Fatalf("Accept bob: %v", err)
	}
	invCarol, err := h.InviteAction("alice", gid, carolID)
	if err != nil {
		t.Fatalf("Invite carol: %v", err)
	}
	if err := h.AcceptInviteAction("carol", invCarol, "alice"); err != nil {
		t.Fatalf("Accept carol: %v", err)
	}

	// Step 3: alice goes offline. Skip pushes to her.
	alice := h.Peer("alice")
	alice.SetOffline(true)

	// Step 4: bob leaves. LeaveGroupAction's fanout
	// reaches carol but NOT alice.
	if err := h.LeaveGroupAction("bob", gid); err != nil {
		t.Fatalf("bob LeaveGroup: %v", err)
	}

	// Step 5: alice reconnects.
	alice.SetOffline(false)

	// Step 6: simulate carol reconnecting to alice by
	// pushing carol's roster AND carol's witness log
	// to alice. Production does both on the inbound
	// "I see you online" handshake (handleInbound calls
	// syncLeaveNoticesToPeer AND syncRostersToPeer on
	// every channel open). v1.2 (2026-07-06): the v1.1.4
	// 19:48 fix used to live in ApplyRosterUpdate's
	// wholesale-overwrite path. v1.2's witness-log
	// refactor moved the fix to the leave-notice path —
	// carol's witness for (g_xxx, bob) replays to alice
	// and alice's ApplyLeaveNotice drops bob. The roster
	// push then becomes a no-op reconciliation.
	h.PushLeaveNoticesAction("carol", gid, []string{"alice"})
	h.PushRosterAction("carol", gid, []string{"alice"})

	// EXPECTED: alice catches up to {alice, carol}.
	snap := h.Snapshot()
	AssertConsistent(t, snap)
	AssertGroupMemberSet(t, snap, "alice", gid, "alice", "carol")
	AssertGroupMemberSet(t, snap, "carol", gid, "alice", "carol")
	AssertGroupAbsent(t, snap, "bob", gid)
}

// TestScenario_CarolAcceptAfterBobLeaves — the 21:08
// inverse.
//
// Timeline:
//  1. alice creates g1, invites bob + carol.
//  2. bob accepts, carol does NOT (still pending).
//  3. bob leaves.
//  4. carol accepts.
//
// EXPECTED: carol's view of g1 is {alice, carol} — NOT
// {alice, bob, carol} (bob already left). The roster
// update path that follows carol's accept must reflect
// the current membership, not the snapshot at invite
// time.
//
// Why this matters: the invite stored a snapshot of
// members at invite time. AcceptGroupInvite must read
// the current state, not the invite's snapshot. v1.1.4
// already handles this; the test guards against a
// regression.
func TestScenario_CarolAcceptAfterBobLeaves(t *testing.T) {
	h := NewHarness(t, []string{"alice", "bob", "carol"})

	bobID, _ := h.ResolvePeerID("bob")
	carolID, _ := h.ResolvePeerID("carol")

	gid, err := h.CreateGroupAction("alice", "g1", []string{bobID, carolID})
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	invBob, err := h.InviteAction("alice", gid, bobID)
	if err != nil {
		t.Fatalf("Invite bob: %v", err)
	}
	if err := h.AcceptInviteAction("bob", invBob, "alice"); err != nil {
		t.Fatalf("Accept bob: %v", err)
	}

	invCarol, err := h.InviteAction("alice", gid, carolID)
	if err != nil {
		t.Fatalf("Invite carol: %v", err)
	}

	if err := h.LeaveGroupAction("bob", gid); err != nil {
		t.Fatalf("bob LeaveGroup: %v", err)
	}

	// Carol accepts the stale invite.
	if err := h.AcceptInviteAction("carol", invCarol, "alice"); err != nil {
		t.Fatalf("Carol accept: %v", err)
	}

	snap := h.Snapshot()
	AssertConsistent(t, snap)
	AssertGroupMemberSet(t, snap, "alice", gid, "alice", "carol")
	AssertGroupMemberSet(t, snap, "carol", gid, "alice", "carol")
	AssertGroupAbsent(t, snap, "bob", gid)
}

// TestScenario_DualLeave — both bob and carol leave
// in quick succession.
//
// EXPECTED: alice ends with {alice}. Both bob and carol
// have the group removed and leavelog entries.
//
// Catches: leave notices interfering with each other
// (e.g. carol's LeaveNotice arriving at alice before
// bob's roster update, then bob's leave notice
// shadowing carol's roster-cleanup step).
func TestScenario_DualLeave(t *testing.T) {
	h := NewHarness(t, []string{"alice", "bob", "carol"})

	bobID, _ := h.ResolvePeerID("bob")
	carolID, _ := h.ResolvePeerID("carol")

	gid, err := h.CreateGroupAction("alice", "g1", []string{bobID, carolID})
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}

	invBob, err := h.InviteAction("alice", gid, bobID)
	if err != nil {
		t.Fatalf("Invite bob: %v", err)
	}
	if err := h.AcceptInviteAction("bob", invBob, "alice"); err != nil {
		t.Fatalf("Accept bob: %v", err)
	}

	invCarol, err := h.InviteAction("alice", gid, carolID)
	if err != nil {
		t.Fatalf("Invite carol: %v", err)
	}
	if err := h.AcceptInviteAction("carol", invCarol, "alice"); err != nil {
		t.Fatalf("Accept carol: %v", err)
	}

	if err := h.LeaveGroupAction("bob", gid); err != nil {
		t.Fatalf("bob LeaveGroup: %v", err)
	}
	if err := h.LeaveGroupAction("carol", gid); err != nil {
		t.Fatalf("carol LeaveGroup: %v", err)
	}

	snap := h.Snapshot()
	AssertConsistent(t, snap)
	AssertGroupMemberSet(t, snap, "alice", gid, "alice")
	AssertGroupAbsent(t, snap, "bob", gid)
	AssertGroupAbsent(t, snap, "carol", gid)
	AssertPeerInLeavelog(t, snap, "bob", gid, true)
	AssertPeerInLeavelog(t, snap, "carol", gid, true)
}