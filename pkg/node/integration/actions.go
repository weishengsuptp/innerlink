// Action wrappers for integration scenarios. These wrap the
// public Node API plus the envelope-synthesis helpers from
// harness.go so scenarios can drive multi-peer mutations in
// one or two calls without thinking about envelope shapes.
//
// Why this is a thin layer: each scenario reads like a
// timeline of human-meaningful events ("alice creates
// 'g1' and invites bob and carol"). If a scenario test
// fails, the first thing we want to look at is which action
// went wrong — so the action set is the natural breakpoint.

package integration_test

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/weishengsuptp/innerlink/internal/protocol"
	"github.com/weishengsuptp/innerlink/pkg/group"
)

// CreateGroupAction creates a group on the given peer and
// returns the rendered GroupID (e.g. "g_<hex>"). The
// "invitees" list is recorded as members-on-paper; the
// caller follows up with InviteAction for each invitee to
// actually emit the invite envelope.
//
// Why split: CreateGroup only writes the creator's local
// state. The invite step is separate because invites may
// race / fail / be retried, and a scenario may want to
// invite one peer now and another later.
func (h *Harness) CreateGroupAction(creatorName, name string, invitees []string) (string, error) {
	p := h.Peer(creatorName)
	if p == nil || p.Node == nil {
		return "", fmt.Errorf("CreateGroupAction: peer %q not running", creatorName)
	}
	gi, err := p.Node.CreateGroup(name, invitees)
	if err != nil {
		return "", fmt.Errorf("peer %s CreateGroup: %w", creatorName, err)
	}
	return gi.GroupID, nil
}

// InviteAction has the creator invite one peer. Returns
// the *group.Invite so the caller can hand it to
// AcceptInviteAction or synthesize envelopes with it.
//
// inviteePeerID is the resolved hex peer ID — NOT the
// peer's friendly name. Callers should look it up via
// h.Peer(name).PeerID() before invoking this action.
//
// Uses BuildInvite (NOT InviteToGroup) because the
// harness is in-process with no TCP channel between
// peers — InviteToGroup would fail the "peer offline
// (no active channel)" check.
func (h *Harness) InviteAction(creatorName, groupID, inviteePeerID string) (*group.Invite, error) {
	p := h.Peer(creatorName)
	if p == nil || p.Node == nil {
		return nil, fmt.Errorf("InviteAction: peer %q not running", creatorName)
	}
	rawID, err := group.ParseGroupID(groupID)
	if err != nil {
		return nil, fmt.Errorf("InviteAction: bad groupID %q: %w", groupID, err)
	}
	inv, err := p.Node.BuildInvite(rawID, inviteePeerID)
	if err != nil {
		return nil, fmt.Errorf("peer %s BuildInvite: %w", creatorName, err)
	}
	return inv, nil
}

// ResolvePeerID returns the hex peer ID of a named peer.
// Convenience for test scenarios that need to pass a
// peer ID string to an action (e.g. InviteAction).
func (h *Harness) ResolvePeerID(name string) (string, error) {
	p := h.Peer(name)
	if p == nil {
		return "", fmt.Errorf("ResolvePeerID: no peer %q", name)
	}
	return p.PeerID(), nil
}

// AcceptInviteAction has the invitee run AcceptGroupInvite
// against a synthesized envelope from the inviter. This
// mimics "the inviter's envelope arrived" — i.e. the
// normal transport path, but driven in-process.
//
// The reason we synthesize the envelope in-process instead
// of building a real channel: the harness is in-process
// with no TCP transport; synthesizing is the cleanest way
// to invoke the receiver-side handler with the right
// shape.
func (h *Harness) AcceptInviteAction(inviteeName string, inv *group.Invite, inviterName string) error {
	invitee := h.Peer(inviteeName)
	if invitee == nil || invitee.Node == nil {
		return fmt.Errorf("AcceptInviteAction: invitee %q not running", inviteeName)
	}
	inviter := h.Peer(inviterName)
	if inviter == nil || inviter.Node == nil {
		return fmt.Errorf("AcceptInviteAction: inviter %q not running", inviterName)
	}
	env := SynthesizeInviteEnvelope(invitee, inv)
	// fromPeerID must be the INVITER's peer ID bytes,
	// not the invitee's. AcceptGroupInvite uses it to
	// look up the inviter's pubkey for signature
	// verification. Passing the invitee's own bytes
	// here makes the lookup fail (no channel state
	// registered for the invitee under themselves).
	if err := invitee.Node.AcceptGroupInvite(env, inviter.PeerIDBytes()); err != nil {
		return fmt.Errorf("peer %s AcceptGroupInvite: %w", inviteeName, err)
	}

	// Production flow: invitee's AcceptGroupInvite sends
	// an accept envelope back to the inviter over a real
	// channel. The inviter's CreatorOnAccept handler then
	// adds the accepter to the roster + broadcasts the
	// updated roster to ALL current members (including
	// the just-joined accepter — they only knew
	// [creator, self] from AcceptGroupInvite).
	//
	// In the harness we have no channel, so we drive
	// CreatorOnAccept directly. This is the missing
	// piece that fixes the "Alice's roster gets clobbered
	// by invitee's stale [creator, self]" bug.
	acceptEnv := SynthesizeAcceptEnvelope(invitee, inv)
	if err := inviter.Node.CreatorOnAccept(acceptEnv, invitee.PeerIDBytes()); err != nil {
		return fmt.Errorf("peer %s CreatorOnAccept: %w", inviterName, err)
	}

	// CreatorOnAccept tried to broadcast to all current
	// members over channels (which we have none of). The
	// harness now drives those receiver-side
	// ApplyRosterUpdate calls directly so every member
	// sees the new roster.
	if _, err := group.ParseGroupID(inv.GroupID); err != nil {
		return fmt.Errorf("AcceptInviteAction: bad groupID in invite: %w", err)
	}
	inviterMembers, err := inviter.Node.ListGroupMembers(inv.GroupID)
	if err != nil {
		return fmt.Errorf("AcceptInviteAction: inviter ListGroupMembers: %w", err)
	}
	for _, mem := range inviterMembers {
		if mem.PeerID == inviter.PeerID() {
			continue // skip self
		}
		target := h.peerByID(mem.PeerID)
		if target == nil || target.Node == nil {
			continue
		}
		if target.IsOffline() {
			continue
		}
		h.PushRosterFromTo(inviterName, inv.GroupID, []string{target.Name})
	}
	// After the invitee accepts, the creator needs to
	// know about it. We synthesize the creator's view
	// by pushing a roster from the invitee to the
	// creator. This mirrors the broadcast that the
	// production node does over its own channel after
	// CreatorOnAccept — but again, in-process without
	// transport.
	//
	// NOTE: in v1.1.4 the creator's ApplyRosterUpdate is
	// idempotent, so pushing twice (once via this helper
	// AND once via the natural fanOut path) is safe.
	creator := h.Peer(inviterName)
	if creator == nil || creator.Node == nil {
		// Best effort — caller may not be running creator.
		return nil
	}
	rawID, err := group.ParseGroupID(inv.GroupID)
	if err != nil {
		return fmt.Errorf("AcceptInviteAction: bad groupID in invite: %w", err)
	}
	m, err := group.LoadMembers(invitee.DataDir, rawID)
	if err != nil {
		return fmt.Errorf("AcceptInviteAction: load invitee members: %w", err)
	}
	h.PushRosterFromTo(inviteeName, inv.GroupID, []string{inviterName})
	// After the creator's roster is updated, broadcast
	// it back to all members (this is what
	// broadcastRosterUpdate does in production). This is
	// the step that fixes the 21:08 re-accept bug —
	// without it, only the creator catches up; bob and
	// carol stay stuck on their old view.
	//
	// We push from the creator (now authoritative) to
	// everyone else who has this group, which includes
	// the just-accepted invitee.
	otherMembers := []string{}
	for _, peer := range h.peers {
		if peer.Name == inviterName || peer.Node == nil {
			continue
		}
		// Only push to peers that actually have the
		// group locally.
		has, err := hasGroup(peer, inv.GroupID)
		if err != nil {
			continue
		}
		if has {
			otherMembers = append(otherMembers, peer.Name)
		}
	}
	if len(otherMembers) > 0 {
		h.PushRosterFromTo(inviterName, inv.GroupID, otherMembers)
	}
	_ = m // silence "declared but not used" — keeping the
	// reference makes the trace easy to follow
	return nil
}

// hasGroup returns true if peer's local members list
// contains groupID. Used by AcceptInviteAction to decide
// which peers to broadcast the roster update to. Also
// returns false if the peer is offline (we'd just buffer
// the push in production).
func hasGroup(p *Peer, groupID string) (bool, error) {
	if p.Node == nil {
		return false, nil
	}
	if p.IsOffline() {
		return false, nil
	}
	memberships, err := p.Node.Memberships()
	if err != nil {
		return false, err
	}
	for _, gid := range memberships {
		if gid == groupID {
			return true, nil
		}
	}
	return false, nil
}

// LeaveGroupAction has a peer call LeaveGroup(renderedID).
// Creator leaves are not supported by Node.LeaveGroup —
// non-creator peer leaves.
//
// What this also does: synthesizes the
// syncLeaveNoticesToPeer envelope to each remaining
// member, mimicking what the production transport
// would have done if the leaving peer were online.
func (h *Harness) LeaveGroupAction(leaverName, groupID string) error {
	p := h.Peer(leaverName)
	if p == nil || p.Node == nil {
		return fmt.Errorf("LeaveGroupAction: peer %q not running", leaverName)
	}
	if err := p.Node.LeaveGroup(groupID); err != nil {
		return fmt.Errorf("peer %s LeaveGroup(%s): %w",
			leaverName, shortGroupID(groupID), err)
	}
	// Synthesize LeaveNotice fan-out to every other peer
	// that has this group. Mirrors what the production
	// syncLeaveNoticesToPeer does over the transport.
	rawID, err := group.ParseGroupID(groupID)
	if err != nil {
		return err
	}
	// Build a LeaveNotice envelope addressed to each
	// remaining member. The dispatcher on each target
	// peer will invoke ApplyLeaveNotice.
	ln := protocol.LeaveNotice{
		GroupID:  hex.EncodeToString(rawID),
		LeaverID: p.PeerID(),
		LeftAt:   time.Now(),
	}
	payload, err := json.Marshal(ln)
	if err != nil {
		return fmt.Errorf("LeaveGroupAction: marshal LeaveNotice: %w", err)
	}
	for _, other := range h.peers {
		if other.Name == leaverName {
			continue
		}
		if other.Node == nil {
			continue
		}
		// Skip offline peers — production would buffer
		// the leave notice until they reconnect, but
		// without transport here we just drop it.
		if other.IsOffline() {
			continue
		}
		// Only fanout to peers that actually have this
		// group locally (no point sending leave notices
		// to a peer that never joined).
		memberships, err := other.Node.Memberships()
		if err != nil {
			h.t.Errorf("LeaveGroupAction: %s.Memberships: %v",
				other.Name, err)
			continue
		}
		hasGroup := false
		for _, gid := range memberships {
			if gid == groupID {
				hasGroup = true
				break
			}
		}
		if !hasGroup {
			continue
		}
		env := protocol.Envelope{
			Version: protocol.ProtocolVersion,
			Type:    protocol.TypeGroupLeaveNotice,
			From:    p.PeerIDBytes(),
			Payload: payload,
			TS:      nowMs(),
		}
		if err := other.Node.ApplyLeaveNotice(env, other.PeerIDBytes()); err != nil {
			h.t.Errorf("LeaveGroupAction: %s.ApplyLeaveNotice from %s: %v",
				other.Name, leaverName, err)
		}
	}
	return nil
}

// PushRosterAction synthesizes a roster update from `fromName`
// to each peer in `toNames`. This is the inverse of
// PushRosterFromTo (which already exists in harness.go)
// — kept as an action for symmetry.
//
// The reason this exists at all: the production system
// auto-pushes roster updates on every group mutation, but
// when peers are running in-process without transport
// connectivity, we have to drive these updates manually.
// PushRosterAction is the "scenario is providing
// connectivity" knob.
func (h *Harness) PushRosterAction(fromName, groupID string, toNames []string) int {
	return h.PushRosterFromTo(fromName, groupID, toNames)
}

// SendGroupMessageAction has a peer send a group message.
// In-process, this only updates the sender's local chat
// log. The harness's drainer goroutines pick up the
// published event so other subscribers can observe it,
// but the actual "message arrived at peer B" loop is
// still manual via the per-scenario dispatch.
//
// Returns the message ID hex string for any follow-up
// assertion (e.g. "B received it").
func (h *Harness) SendGroupMessageAction(senderName, groupID, text string) (string, error) {
	p := h.Peer(senderName)
	if p == nil || p.Node == nil {
		return "", fmt.Errorf("SendGroupMessageAction: peer %q not running", senderName)
	}
	rawID, err := group.ParseGroupID(groupID)
	if err != nil {
		return "", fmt.Errorf("SendGroupMessageAction: bad groupID: %w", err)
	}
	if err := p.Node.SendGroupMessage(rawID, text); err != nil {
		return "", fmt.Errorf("peer %s SendGroupMessage: %w", senderName, err)
	}
	return "", nil // msg ID is implementation detail of SendGroupMessage
}

// RestartPeerAction simulates a process crash + restart:
// Close() the peer, then bring it back up at the same
// DataDir. The peer's PeerID stays the same (it's derived
// from device.key which is in the DataDir).
//
// This is the simplest way to model "the user closed the
// app and reopened it." The Next/SelfPeerID etc. surface
// remains stable.
func (h *Harness) RestartPeerAction(name string) error {
	p := h.Peer(name)
	if p == nil {
		return fmt.Errorf("RestartPeerAction: no peer %q", name)
	}
	return p.Restart()
}

// nowMs returns unix milliseconds. Used for envelope
// timestamps when synthesizing. Lives here to keep
// actions.go self-contained.
func nowMs() int64 {
	return time.Now().UnixMilli()
}