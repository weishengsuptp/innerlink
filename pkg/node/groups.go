package node

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	ic "github.com/weishengsuptp/innerlink/internal/crypto"
	"github.com/weishengsuptp/innerlink/internal/filetransfer"
	"github.com/weishengsuptp/innerlink/internal/leavelog"
	"github.com/weishengsuptp/innerlink/internal/protocol"
	"github.com/weishengsuptp/innerlink/internal/storage"
	"github.com/weishengsuptp/innerlink/pkg/group"
)

// GroupInfo is the public-facing summary of one group, returned
// to the GUI / CLI for display.
type GroupInfo struct {
	GroupID   string    `json:"group_id"`   // rendered "g_<hex>"
	GroupName string    `json:"group_name"`
	Creator   string    `json:"creator"`    // creator's peerID
	CreatedAt time.Time `json:"created_at"`
	Members   []string  `json:"members"`    // peerID hex strings, sorted
	Self      bool      `json:"self"`       // true if our peerID is a member
}

// acceptPayload is the JSON shape of a TypeGroupInviteAccept
// envelope's Payload. It echoes back the rendered GroupID and
// the original Invite.Nonce so the inviter can correlate
// the accept with the original invite.
type acceptPayload struct {
	GroupID string `json:"group_id"`
	Nonce   []byte `json:"nonce"`
}

// declinePayload is the JSON shape of a TypeGroupInviteDecline
// envelope's Payload. Reason is optional; "" means "no reason".
type declinePayload struct {
	GroupID string `json:"group_id"`
	Reason  string `json:"reason,omitempty"`
}

// groupMu protects n.groupsByID — populated by JoinGroup and
// CreateGroup, read by SendGroupMessage and HistoryGroup. Per-
// group operations (append to chat.enc, members.json updates)
// take their own per-group lock.
var groupMu sync.Mutex

// (No struct field — Node.groupsByID is added below in the
//  patch to node.go; for now we keep it minimal and look up
//  everything via the storage layer on every call.)

// dataDir returns the on-disk data directory used by this Node
// (we use the layout's DataDir, same as chatStore.SaveDir()).
func (n *Node) dataDir() string {
	return n.layout.DataDir
}

// CreateGroup makes a new group with the creator as the first
// member. Member peerIDs may be empty (you can create a solo
// group and invite people later via InviteToGroup). Returns
// the GroupInfo on success.
//
// On-disk layout created here:
//   <dataDir>/groups/<renderedID>/
//     ├── members.json     (creator as first member)
//     └── chat.enc         (empty)
//
// The creator's SenderKey is NOT generated here — that needs
// the creator's SM2 private key, which we don't have access to
// from outside the identity package without exposing more API.
// SenderKeys land in a follow-up commit that wires identity
// into pkg/group.SenderKey generation. For now, plain group
// messages are broadcast unencrypted (acceptable for v1.1
// since the threat model is "LAN passive observer"; per-member
// channel encryption still protects against that).
func (n *Node) CreateGroup(name string, memberPeerIDs []string) (*GroupInfo, error) {
	if n.id == nil {
		return nil, errors.New("node: not started")
	}
	name = strings_TrimSpace(name)
	if name == "" {
		return nil, errors.New("node: CreateGroup: name is empty")
	}
	if len(name) > 30 {
		return nil, errors.New("node: CreateGroup: name too long (max 30 chars)")
	}
	if len(memberPeerIDs) > 50 {
		// hard cap way above design target (20) — gives
		// room for accidental miscounts without silently
		// truncating the member list.
		return nil, errors.New("node: CreateGroup: too many members (max 50)")
	}

	now := time.Now().UTC()
	selfHex := n.id.PeerIDHex()
	creator := n.id.PeerID() // 16 raw bytes for GroupID input

	// GroupID is content-addressed. Same creator + name +
	// timestamp → same ID. Two creators can't collide on
	// name+timestamp because their creator peerIDs differ.
	rawID := group.ComputeGroupID(creator, name, now)
	rendered := group.RenderGroupID(rawID)

	// Build members.json with ONLY the creator. Invitees
	// are added by CreatorOnAccept when each invitee
	// actually accepts — pre-listing them here would make
	// InviteToGroup's `m.Contains(invitee)` check
	// (groups.go) return true and reject the freshly-added
	// invitee with "invitee already a member". The
	// proper "joined" moment is when the accept arrives.
	members := []group.Member{
		{PeerID: selfHex, Alias: n.GetSelfAlias(), JoinedAt: now, IsCreator: true},
	}
	m := &group.Members{
		GroupID:   rendered,
		GroupName: name,
		Creator:   selfHex,
		CreatedAt: now,
		Members:   members,
	}
	if err := m.Save(n.dataDir(), rawID); err != nil {
		return nil, fmt.Errorf("node: CreateGroup save members.json: %w", err)
	}

	// Create an empty chat.enc so HistoryGroup doesn't
	// return "(no history)" indefinitely. AppendGroup on
	// a fresh group would auto-create it, but doing it
	// here makes the on-disk shape predictable.
	if err := n.chatStore.AppendGroup(rendered, &storage.Record{
		Timestamp: now,
		From:      selfHex,
		To:        "",
		Direction: "system",
		Body:      "群已创建",
		GroupID:   rendered,
		MsgID:     "",
	}); err != nil {
		// Non-fatal: even if AppendGroup fails, members.json
		// is on disk and the group is usable.
		log.Printf("[WARN  ] CreateGroup: seed chat.enc: %v", err)
	}

	log.Printf("[GROUP ] created group=%s name=%q members=%d", rendered, name, len(members))
	// v1.1 (2026-06-28): tell the GUI a new group exists
	// so its sidebar refreshes without waiting for the
	// next peer:event. InviterHex is empty for self-create.
	n.publishGroupEvent(GroupEvent{
		Type:      GroupAdded,
		GroupID:   rendered,
		GroupName: name,
	})
	return n.toGroupInfo(m, rawID, true), nil
}

// Memberships returns the rendered GroupID ("g_<hex>")
// of every group this peer currently knows about, sorted
// for stable comparison. Useful for tests and for the
// frontend side-bar; production code mostly uses
// ListGroups for the full GroupInfo view.
func (n *Node) Memberships() ([]string, error) {
	renderedIDs, err := n.chatStore.ListGroups()
	if err != nil {
		return nil, err
	}
	out := make([]string, len(renderedIDs))
	copy(out, renderedIDs)
	sort.Strings(out)
	return out, nil
}

// ListGroups enumerates every group on disk and returns its
// GroupInfo. Sorted by GroupID for stable display.
func (n *Node) ListGroups() ([]*GroupInfo, error) {
	renderedIDs, err := n.chatStore.ListGroups()
	if err != nil {
		return nil, err
	}
	out := make([]*GroupInfo, 0, len(renderedIDs))
	for _, rendered := range renderedIDs {
		rawID, err := group.ParseGroupID(rendered)
		if err != nil {
			log.Printf("[WARN  ] ListGroups: bad GroupID %q: %v", rendered, err)
			continue
		}
		m, err := group.LoadMembers(n.dataDir(), rawID)
		if err != nil {
			log.Printf("[WARN  ] ListGroups: load %s: %v", rendered, err)
			continue
		}
		out = append(out, n.toGroupInfo(m, rawID, m.Contains(n.id.PeerIDHex())))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GroupID < out[j].GroupID })
	return out, nil
}

// GetGroup returns one group's GroupInfo or nil + ErrNotExist.
// Used by the GUI sidebar to refresh a single group without
// re-walking every group's members.json.
func (n *Node) GetGroup(renderedID string) (*GroupInfo, error) {
	rawID, err := group.ParseGroupID(renderedID)
	if err != nil {
		return nil, err
	}
	m, err := group.LoadMembers(n.dataDir(), rawID)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrGroupNotFound
		}
		return nil, err
	}
	return n.toGroupInfo(m, rawID, m.Contains(n.id.PeerIDHex())), nil
}

// ErrGroupNotFound is returned by GetGroup when the group
// doesn't exist on this peer. Exposed as a sentinel so the
// GUI can distinguish "never joined" from "real error".
var ErrGroupNotFound = errors.New("node: group not found")

// errAlreadyMember is a sentinel returned from inside an
// UpdateMembers callback to signal "no change needed".
// CreatorOnAccept uses this to short-circuit the
// already-member case (which the previous LoadMembers +
// Contains check handled before the per-group lock was
// added).
var errAlreadyMember = errors.New("node: already a member")

// InviteToGroup signs an Invite for inviteePeerID and sends it
// over the 1:1 channel to that peer. The invitee's AcceptGroup
// response will trigger this creator's "add member" flow.
//
// Returns the Invite so the caller can show it in the UI for
// confirmation / replay protection.
//
// sender (inviter) defaults to the local peerID; can be
// overridden by tests.
func (n *Node) InviteToGroup(rawGroupID []byte, inviteePeerID string) (*group.Invite, error) {
	inv, err := n.BuildInvite(rawGroupID, inviteePeerID)
	if err != nil {
		return nil, err
	}
	rendered := group.RenderGroupID(rawGroupID)
	payload, err := json.Marshal(inv)
	if err != nil {
		return nil, fmt.Errorf("node: InviteToGroup marshal: %w", err)
	}

	// 1:1 send via the existing channel. If the peer isn't
	// online (no channel), the sender's underlying Channel
	// path returns an error — caller can retry once the
	// peer is reachable.
	pid, err := hexToBytes(inviteePeerID)
	if err != nil {
		return nil, fmt.Errorf("node: InviteToGroup peer id: %w", err)
	}
	st := n.channels.get(pid)
	if st == nil {
		return nil, errors.New("node: InviteToGroup: peer offline (no active channel)")
	}
	if err := st.ch.Send(n.ctx, protocol.Envelope{
		Type:    protocol.TypeGroupInvite,
		Payload: payload,
	}); err != nil {
		return nil, fmt.Errorf("node: InviteToGroup send: %w", err)
	}
	log.Printf("[GROUP ] invited %s to %s (nonce=%x)", inviteePeerID, rendered, inv.Nonce[:4])
	return inv, nil
}

// BuildInvite constructs + signs the Invite envelope
// without sending it. Used by the production InviteToGroup
// (which then sends it) and by the integration test
// harness (which delivers the invite in-process, bypassing
// the channel lookup).
//
// Both inviter and invitee are validated as before. The
// only thing this skips relative to InviteToGroup is the
// network send.
func (n *Node) BuildInvite(rawGroupID []byte, inviteePeerID string) (*group.Invite, error) {
	if n.id == nil {
		return nil, errors.New("node: not started")
	}
	m, err := group.LoadMembers(n.dataDir(), rawGroupID)
	if err != nil {
		return nil, fmt.Errorf("node: BuildInvite load: %w", err)
	}
	if inviteePeerID == "" {
		return nil, errors.New("node: BuildInvite: invitee empty")
	}
	// The inviter MUST be a current member (we don't allow
	// random peers to spam invites for groups they don't
	// belong to).
	selfHex := n.id.PeerIDHex()
	if !m.Contains(selfHex) {
		return nil, errors.New("node: BuildInvite: inviter is not a member of this group")
	}
	if m.Contains(inviteePeerID) {
		return nil, errors.New("node: BuildInvite: invitee already a member")
	}

	inv := group.NewInvite(rawGroupID, m.GroupName, m.Creator, selfHex, time.Now().UTC())
	// Sign with our device Identity. We can't call
	// inv.Sign(*sm2.PrivateKey) directly because internal/identity
	// doesn't expose the raw key — but Identity.Sign produces
	// the same SM2-with-SM3 signature that pkg/group.Verify
	// expects. We invoke the canonicalization exposed by
	// pkg/group and copy the bytes.
	sig, err := n.id.Sign(inv.Canonical())
	if err != nil {
		return nil, fmt.Errorf("node: BuildInvite sign: %w", err)
	}
	inv.Signature = sig
	return inv, nil
}

// AcceptGroupInvite is called by the dispatcher when a
// TypeGroupInvite envelope arrives. Verifies the invite
// signature against the inviter's SM2 public key, then:
//   1. Saves a local members.json (so ListGroups shows it)
//   2. Sends TypeGroupInviteAccept back to the inviter
//
// The inviter's handler (CreatorOnAccept) does the actual
// roster update + SenderKey distribution.
func (n *Node) AcceptGroupInvite(env protocol.Envelope, fromPeerID []byte) error {
	if n.id == nil {
		return errors.New("node: not started")
	}
	var inv group.Invite
	if err := json.Unmarshal(env.Payload, &inv); err != nil {
		return fmt.Errorf("node: AcceptGroupInvite unmarshal: %w", err)
	}
	if inv.Expiry(24 * time.Hour) {
		return errors.New("node: AcceptGroupInvite: invite expired (>24h)")
	}
	// Verify against the inviter's SM2 public key. The
	// handshake already gave us their 64-byte pubkey; we
	// unmarshal to the gmsm form Verify expects.
	inviterHex := peerBytesToHex(fromPeerID)
	pubBytes, err := n.lookupPeerPublicKey(inviterHex)
	if err != nil {
		return fmt.Errorf("node: AcceptGroupInvite: lookup inviter pub: %w", err)
	}
	pub, err := sm2Unmarshal(pubBytes)
	if err != nil {
		return fmt.Errorf("node: AcceptGroupInvite: unmarshal inviter pub: %w", err)
	}
	if err := inv.Verify(pub); err != nil {
		return fmt.Errorf("node: AcceptGroupInvite: %w", err)
	}

	rawID, err := group.ParseGroupID(inv.GroupID)
	if err != nil {
		return fmt.Errorf("node: AcceptGroupInvite: bad GroupID: %w", err)
	}
	now := time.Now().UTC()
	selfHex := n.id.PeerIDHex()

	// Create local members.json. If we already have it
	// (re-accept after a restart), preserve existing
	// members and update our entry's JoinedAt.
	m, err := group.LoadMembers(n.dataDir(), rawID)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("node: AcceptGroupInvite load: %w", err)
		}
		// Fresh — create from invite.
		m = &group.Members{
			GroupID:   inv.GroupID,
			GroupName: inv.GroupName,
			Creator:   inv.Creator,
			CreatedAt: inv.IssuedAt,
			Members: []group.Member{
				{PeerID: inv.Creator, Alias: "", JoinedAt: inv.IssuedAt, IsCreator: true},
				{PeerID: selfHex, Alias: n.GetSelfAlias(), JoinedAt: now},
			},
		}
	} else {
		// Already a member? Idempotent — re-send accept and
		// return success.
		if m.Contains(selfHex) {
			log.Printf("[GROUP ] re-accept on already-joined group=%s", inv.GroupID)
			return n.sendAcceptResponse(inv, fromPeerID)
		}
		// Add ourselves.
		_ = m.AddMember(group.Member{PeerID: selfHex, Alias: n.GetSelfAlias(), JoinedAt: now})
	}
	if err := m.Save(n.dataDir(), rawID); err != nil {
		return fmt.Errorf("node: AcceptGroupInvite save: %w", err)
	}
	// v1.1.4 (2026-07-02, second hotfix): clear the
	// leavelog entry for this group. The user-reported
	// 21:08 bug was a re-accept-after-leave regression:
	// B left g_aba923 at 21:05:33 (leavelog gets the
	// entry), then was re-invited at 21:05:58. This
	// AcceptGroupInvite wrote a fresh 1-member
	// members.json correctly, but the post-accept
	// 3-member roster push from the creator was
	// silently skipped by ApplyRosterUpdate because
	// the leavelog still held the prior leave. B's
	// local stayed at 1 member while the rest of the
	// group saw 3.
	//
	// The intent of "I left this group" is revoked
	// the moment the peer re-accepts an invite. Drop
	// the entry here + Save so the next handshake's
	// syncLeaveNoticesToPeer doesn't re-broadcast a
	// stale leave notice either.
	if n.leavelog != nil {
		if err := n.leavelog.Remove(inv.GroupID); err != nil {
			log.Printf("[WARN ] leavelog: remove on re-accept %s: %v", inv.GroupID, err)
		} else if err := n.leavelog.Save(); err != nil {
			log.Printf("[WARN ] leavelog: save on re-accept %s: %v", inv.GroupID, err)
		}
	}
	log.Printf("[GROUP ] accepted invite to %s (inviter=%s)", inv.GroupID, inviterHex)
	// v1.1 (2026-06-29) hotfix: seed chat.enc so ListGroups
	// (which filters by chat.enc existence in storage/group.go)
	// returns this group on the receiver's sidebar. Without
	// this seed, members.json alone isn't enough — the
	// receiver's sidebar would refuse to show the group until
	// the very first message arrives (AppendGroup creates
	// chat.enc lazily on first write). The user-facing
	// symptom: "I created a group, but it doesn't auto-appear
	// on the invitee's side" — because the invitee sees no
	// chat.enc, so the sidebar refresh post-GroupAdded
	// returned an empty list. Mirrors CreateGroup's seed
	// pattern (it appends the same "system" record for the
	// same reason).
	if err := n.chatStore.AppendGroup(inv.GroupID, &storage.Record{
		Timestamp: now,
		From:      selfHex,
		To:        "",
		Direction: "system",
		Body:      "已加入群聊",
		GroupID:   inv.GroupID,
		MsgID:     "",
	}); err != nil {
		// Non-fatal: even without chat.enc, members.json +
		// the group:event below still let the user access
		// the group once a first message creates chat.enc.
		log.Printf("[WARN  ] AcceptGroupInvite: seed chat.enc: %v", err)
	}
	// v1.1 (2026-06-28): notify the GUI a new group exists
	// so the sidebar refreshes and the "你被拉进群 X"
	// toast fires. InviterHex is the peer's 32-char hex
	// (not raw bytes) so the frontend can use it directly
	// to look up the inviter's alias in its peer roster.
	n.publishGroupEvent(GroupEvent{
		Type:       GroupAdded,
		GroupID:    inv.GroupID,
		GroupName:  inv.GroupName,
		InviterHex: inviterHex,
	})

	// Send accept back to the inviter.
	return n.sendAcceptResponse(inv, fromPeerID)
}

// sendAcceptResponse sends a TypeGroupInviteAccept envelope
// to the inviter with our echoed GroupID + the original
// Invite.Nonce for correlation.
func (n *Node) sendAcceptResponse(inv group.Invite, inviterPeerID []byte) error {
	payload, err := json.Marshal(acceptPayload{
		GroupID: inv.GroupID,
		Nonce:   inv.Nonce,
	})
	if err != nil {
		return err
	}
	st := n.channels.get(inviterPeerID)
	if st == nil || st.ch == nil {
		// Inviter went offline between invite and accept,
		// OR we're running under the integration test
		// harness where channelState was seeded with
		// pubkey-only (no real ch). Either way, drop the
		// outbound accept — the harness will invoke
		// CreatorOnAccept directly to advance state.
		log.Printf("[WARN  ] sendAcceptResponse: inviter %x offline; accept dropped", inviterPeerID)
		return nil
	}
	return st.ch.Send(n.ctx, protocol.Envelope{
		Type:    protocol.TypeGroupInviteAccept,
		Payload: payload,
	})
}

// DeclineGroupInvite sends a decline envelope so the inviter
// can show "X declined" without polling. The invite is
// dropped locally regardless of send success (decline is
// best-effort).
func (n *Node) DeclineGroupInvite(env protocol.Envelope, fromPeerID []byte, reason string) error {
	var inv group.Invite
	if err := json.Unmarshal(env.Payload, &inv); err != nil {
		return err
	}
	payload, err := json.Marshal(declinePayload{
		GroupID: inv.GroupID,
		Reason:  reason,
	})
	if err != nil {
		return err
	}
	st := n.channels.get(fromPeerID)
	if st == nil || st.ch == nil {
		// Integration-test-harness path: no real
		// channel exists (pubkey was seeded without
		// ch). Drop the outbound ack; the harness
		// doesn't model the decline-ack round-trip.
		log.Printf("[GROUP ] declined invite to %s from %x (no channel — ack dropped)", inv.GroupID, fromPeerID)
		return nil
	}
	log.Printf("[GROUP ] declined invite to %s from %x", inv.GroupID, fromPeerID)
	return st.ch.Send(n.ctx, protocol.Envelope{
		Type:    protocol.TypeGroupInviteDecline,
		Payload: payload,
	})
}

// CreatorOnAccept is the dispatcher-side handler for
// TypeGroupInviteAccept on the inviter. Adds the accepter
// to members.json + SenderKeys distribution (TODO when
// SenderKeys land).
//
// Concurrency: the read-modify-write of members.json is
// wrapped in group.UpdateMembers, which holds a per-group
// lock for the entire operation. Without this, two
// accept envelopes arriving simultaneously would each
// read {alice}, each add their own accepter, and the
// second Save would clobber the first — losing one
// member. Caught by S5 in the integration test harness
// (2026-07-03).
func (n *Node) CreatorOnAccept(env protocol.Envelope, fromPeerID []byte) error {
	var ap acceptPayload
	if err := json.Unmarshal(env.Payload, &ap); err != nil {
		return fmt.Errorf("node: CreatorOnAccept unmarshal: %w", err)
	}
	rawID, err := group.ParseGroupID(ap.GroupID)
	if err != nil {
		return fmt.Errorf("node: CreatorOnAccept bad GroupID: %w", err)
	}
	accepterHex := peerBytesToHex(fromPeerID)
	now := time.Now().UTC()

	// Atomic read-modify-write under the per-group lock.
	m, err := group.UpdateMembers(n.dataDir(), rawID, func(m *group.Members) error {
		if m.Contains(accepterHex) {
			// Already a member — leave the file as-is
			// (the caller will treat the no-op return
			// below as "no change, no broadcast").
			return errAlreadyMember
		}
		return m.AddMember(group.Member{
			PeerID:   accepterHex,
			JoinedAt: now,
		})
	})
	if errors.Is(err, errAlreadyMember) {
		log.Printf("[GROUP ] accept from %s: already member, no-op", accepterHex)
		return nil
	}
	if err != nil {
		return fmt.Errorf("node: CreatorOnAccept: %w", err)
	}
	log.Printf("[GROUP ] %s accepted invite to %s; roster updated", accepterHex, ap.GroupID)
	// v1.1.1 (2026-06-29): broadcast the updated roster to
	// every member (including the just-joined accepter —
	// they only know [creator, self] from AcceptGroupInvite
	// and need the full roster too). Best-effort: a failed
	// broadcast to one peer doesn't roll back the accept —
	// that peer just stays stale until the next roster-
	// changing event.
	n.broadcastRosterUpdate(m)
	// v1.1.1 (2026-06-29): also tell the creator's own
	// GUI to re-read the roster — the broadcast above
	// doesn't include self (self already has the latest
	// from the local Save), so without this explicit
	// GroupUpdated, the creator's sidebar would freeze
	// at "1 成员" forever (the snapshot at CreateGroup
	// time). The frontend reloads ListGroups on every
	// GroupUpdated, which fixes both the sidebar count
	// AND the settings panel's member list.
	n.publishGroupEvent(GroupEvent{
		Type:      GroupUpdated,
		GroupID:   m.GroupID,
		GroupName: m.GroupName,
	})
	// TODO: distribute SenderKey for accepter here. For
	// now we broadcast plain (channel-encrypted) group
	// messages, so no SenderKey handshake is required.
	return nil
}

// SendGroupMessage broadcasts text to every online member of
// the group (peer who currently has an active channel). The
// payload is sent as a TypeText envelope with GroupID set in
// the envelope header — recipients route by GroupID, not by
// peerID. Each member's chat history persists in their own
// groups/<id>/chat.enc.
//
// Plaintext for now — SenderKeys encryption lands in a
// follow-up.
func (n *Node) SendGroupMessage(rawGroupID []byte, text string) error {
	if n.id == nil {
		return errors.New("node: not started")
	}
	if text == "" {
		return errors.New("node: SendGroupMessage: empty text")
	}
	rendered := group.RenderGroupID(rawGroupID)
	m, err := group.LoadMembers(n.dataDir(), rawGroupID)
	if err != nil {
		return fmt.Errorf("node: SendGroupMessage load: %w", err)
	}
	selfHex := n.id.PeerIDHex()
	if !m.Contains(selfHex) {
		return errors.New("node: SendGroupMessage: sender is not a member")
	}
	now := time.Now().UTC()

	// Persist locally first (we wrote it, we should see it).
	rec := &storage.Record{
		Timestamp: now,
		From:      selfHex,
		To:        "", // no specific recipient in a group
		Direction: "out",
		Body:      text,
		GroupID:   rendered,
		MsgID:     "",
	}
	if err := n.chatStore.AppendGroup(rendered, rec); err != nil {
		return fmt.Errorf("node: SendGroupMessage append: %w", err)
	}
	// Also surface on the in-memory history list so the
	// GUI's History(peerRef) call sees group messages too
	// (we use a synthetic "peerRef" = groupID for routing
	// in the in-memory slice).
	n.appendHistory(rec)
	n.publishMessage(Message{
		PeerID: rendered, Body: text,
		Timestamp: now, Direction: "out",
	})

	// Build the envelope ONCE, send to every member.
	// Per-member sends share the same envelope bytes — the
	// only thing that differs is the Channel (different
	// session keys per peer).
	env := protocol.Envelope{
		Type:    protocol.TypeText,
		GroupID: rawGroupID,
		Payload: []byte(text),
	}
	delivered := 0
	skippedOffline := 0
	for _, mem := range m.Members {
		if mem.PeerID == selfHex {
			continue // skip self
		}
		pid, err := hexToBytes(mem.PeerID)
		if err != nil {
			log.Printf("[WARN  ] SendGroupMessage: bad member id %q: %v", mem.PeerID, err)
			continue
		}
		st := n.channels.get(pid)
		if st == nil || st.ch == nil {
			// Member offline (no active TCP channel)
			// OR integration-test-harness stub channel
			// (pubkey seeded without ch). Either way,
			// drop the outbound message and let the
			// harness drive the receiver directly if
			// needed.
			//
			// Pre-fix: dropped silently. Post-fix
			// (v1.1.1, 2026-06-30): if the roster has
			// an Addrs[] entry for this peer, fire a
			// best-effort dialAddr() in the background
			// so the NEXT message reaches them. The
			// CURRENT message still drops here — we
			// don't wait for the dial (it could take
			// 100 ms to 5 s depending on network).
			// This is the fix for "VM-to-VM group
			// messages don't cross" — on different
			// subnets the VMs never directly dial
			// each other, only A. Without this, a
			// broadcast sent by VM-B to anyone other
			// than A silently drops.
			if entry, eerr := n.rosterStore.Get(mem.PeerID); eerr == nil && entry.Addrs != nil && len(entry.Addrs) > 0 {
				addr := entry.Addrs[0]
				log.Printf("[GROUP ] no channel to %s @ %s, firing best-effort dial for NEXT message", mem.PeerID, addr)
				n.dialAddr(addr)
			} else {
				log.Printf("[GROUP ] no channel to %s and no roster addr (member offline / unknown IP), message drops", mem.PeerID)
			}
			skippedOffline++
			continue
		}
		if err := st.ch.Send(n.ctx, env); err != nil {
			log.Printf("[WARN  ] SendGroupMessage send to %s: %v", mem.PeerID, err)
			continue
		}
		delivered++
	}
	log.Printf("[GROUP ] sent to %s: %d/%d delivered, %d dropped (offline, dial fired) (out)",
		rendered, delivered, len(m.Members)-1, skippedOffline)
	return nil
}

// HistoryGroup returns the chat records for a group, oldest
// first. Returns an empty slice if the group has no chat
// records yet (just-created groups).
func (n *Node) HistoryGroup(renderedID string) ([]Message, error) {
	recs, err := n.chatStore.HistoryGroup(renderedID)
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(recs))
	for _, r := range recs {
		// v1.1 (2026-06-28) hotfix: populate SenderID from
		// the record's From so the history drawer can
		// render the actual member name per row instead
		// of falling back to the conversation group name.
		// Without this, every group message reloaded from
		// disk has empty SenderID, and the drawer's
		// per-row sender lookup hits the fallback path
		// → "(未知成员)" for inbound or "我" for outbound
		// (fine) but loses the per-member distinction.
		// Outbound records have From = selfHex too, so we
		// gate by Direction — only inbound messages are
		// not self-typed, hence only those need SenderID.
		var senderID string
		if r.Direction == "in" {
			senderID = r.From
		}
		out = append(out, Message{
			PeerID:    renderedID,
			SenderID:  senderID,
			Body:      r.Body,
			Timestamp: r.Timestamp,
			Direction: r.Direction,
			LocalPath: r.LocalPath,
		})
	}
	return out, nil
}

// deleteGroupDirsLocal removes BOTH the chat.enc + sender-keys
// directory (via chatStore.DeleteGroup, which targets
// <chatStore.SaveDir()>/groups/<gid> = <saveDir>/chat/groups/<gid>)
// AND the members.json directory (n.dataDir()/groups/<gid>,
// a different root because chatStore opens saveDir under an
// extra "chat/" layer). The chat package's DeleteGroup only
// touches the chat side — without this helper LeaveGroup
// left members.json behind on the leaver's disk, which made
// a second LeaveGroup see self still listed (it appeared to
// "succeed" forever) and left stale disk state for future
// CreateGroup that happened to land on the same GroupID.
//
// v1.1.2 (2026-06-30) — pinned down by the user-reported
// "solo creator can't leave group" bug: chatStore.DeleteGroup
// alone was insufficient.
func (n *Node) deleteGroupDirsLocal(renderedID string) error {
	membersDir := filepath.Join(n.dataDir(), storage.GroupDirName, renderedID)
	if err := os.RemoveAll(membersDir); err != nil {
		return fmt.Errorf("node: remove members dir %s: %w", membersDir, err)
	}
	if err := n.chatStore.DeleteGroup(renderedID); err != nil {
		return fmt.Errorf("node: chatStore.DeleteGroup: %w", err)
	}
	return nil
}

// LeaveGroup removes self from the group's roster and
// deletes the local directory (chat.enc + members.json).
// Every REMAINING member receives the new roster via
// broadcastRosterUpdate; their ApplyRosterUpdate handler
// rebuilds their local members.json without self and
// publishes GroupUpdated so the sidebar's "<N> 成员"
// count drops. v1.1.2 (2026-06-30): the prior implementation
// only dropped self locally and skipped the broadcast —
// "3 在线" then stayed stuck on every remaining member's
// sidebar until they restarted. The matching fix on the
// receiver side is ApplyRosterUpdate, which only needs the
// post-leave roster to do its job (no separate leave
// envelope type — roster diff is enough).
//
// Three creator-leave branches:
//   - creator + ≥1 other member → REJECT (orphan on the
//     creator's side; the proper path is a future
//     DissolveGroup broadcast — v1.1.x TODO).
//   - creator + alone (only self in roster) → SELF-DISSOLVE
//     (delete chat.enc + members.json, publish
//     GroupRemoved). This branch exists because
//     Members.RemoveMember protects the creator from being
//     removed (a defensive guard against accidental group
//     orphaning), so without this special-case the empty-
//     members delete branch below is unreachable for solo
//     creators and LeaveGroup errors out with
//     "RemoveMember returned false" — the user-reported
//     "solo creator can't leave" bug. v1.1.2 (2026-06-30)
//     hotfix.
//   - non-creator → standard RemoveMember + broadcast
//     roster to remaining peers.
//
// Offline-receiver caveat: if a remaining member has no
// active channel at leave time, broadcastRosterUpdate
// best-effort drops them (same as SetGroupName/Remark —
// see notes in broadcastRosterUpdate). They'll re-sync on
// next reconnect via a future TypeGroupRosterSync-pull
// API (v1.1.x TODO). This is not new in 1.1.2 — the same
// caveat applies to the accept path CreatorOnAccept.
func (n *Node) LeaveGroup(renderedID string) error {
	rawID, err := group.ParseGroupID(renderedID)
	if err != nil {
		return err
	}
	m, err := group.LoadMembers(n.dataDir(), rawID)
	if err != nil {
		return err
	}
	selfHex := n.id.PeerIDHex()
	if !m.Contains(selfHex) {
		return errors.New("node: LeaveGroup: not a member")
	}
	// Creator-leave branches (see function doc).
	// v1.1 (2026-06-28, hotfix): allow the creator to leave
	// when they're the SOLE remaining member — this is
	// effectively a self-dissolve (the empty-members branch
	// below deletes the local group). When other members
	// remain, the creator still can't leave: doing so would
	// orphan the group on their end while leaving stale
	// creator entries in everyone else's roster. The proper
	// path for "I'm done with this group and want everyone
	// out" is a DissolveGroup broadcast (v1.1.x TODO).
	if m.Creator == selfHex {
		if len(m.Members) > 1 {
			return errors.New("node: LeaveGroup: 群主无法直接退出，请等待其他成员先退出或解散群聊（v1.1.x TODO DissolveGroup）")
		}
		// v1.1.2 (2026-06-30) hotfix: solo creator self-dissolve.
		// Members.RemoveMember protects the creator from being
		// removed (so an accidental RemoveMember(self) call can't
		// orphan the group), which means the empty-members
		// branch below is unreachable when self is the only
		// member AND the creator. Without this explicit path,
		// the user gets the cryptic "RemoveMember returned false"
		// error and the group can't be left at all.
		if err := n.deleteGroupDirsLocal(renderedID); err != nil {
			return err
		}
		log.Printf("[GROUP ] solo creator self-dissolved group=%s", renderedID)
		n.publishGroupEvent(GroupEvent{
			Type:      GroupRemoved,
			GroupID:   renderedID,
			GroupName: m.GroupName,
		})
		return nil
	}
	// Non-creator path.
	if !m.RemoveMember(selfHex) {
		return errors.New("node: LeaveGroup: RemoveMember returned false")
	}
	// If the group is empty after our removal, delete it.
	// Otherwise save the updated roster and broadcast the
	// new roster to remaining members (v1.1.2 hotfix —
	// before this, remaining peers kept a stale "<N> 成员
	// / <N> 在线" until restart).
	//
	// v1.1.2: chatStore.DeleteGroup alone only removes
	// chat.enc; members.json sits under a different root
	// (see deleteGroupDirsLocal). Use the helper so the
	// second LeaveGroup actually sees "not a member" instead
	// of looping through this branch forever.
	if len(m.Members) == 0 {
		return n.deleteGroupDirsLocal(renderedID)
	}
	if err := m.Save(n.dataDir(), rawID); err != nil {
		return err
	}
	// Delete our local chat.enc too — we shouldn't see
	// messages we left behind (matches Signal/WhatsApp
	// "leave and forget" semantics).
	if err := n.chatStore.DeleteGroup(renderedID); err != nil {
		return err
	}
	log.Printf("[GROUP ] left group=%s (local cleanup done, broadcasting roster to %d remaining)",
		renderedID, len(m.Members))
	// v1.1.2 (2026-06-30): the v1.1.1 fix landed broadcastRosterUpdate
	// on CreatorOnAccept but forgot this path. Mirror it: tell
	// the remaining peers so their members.json gets rebuilt
	// without us. broadcastRosterUpdate skips self internally
	// and best-effort drops offline peers (same semantics as
	// the accept path — see agent memory entry on roster
	// write-broadcast exclusions).
	n.broadcastRosterUpdate(m)
	// v1.1.4 (2026-07-02): persist the leave to the
	// device-level leavelog so we can replay a
	// TypeGroupLeaveNotice to the creator on a future
	// handshake. The creator may have been offline at
	// leave time (the broadcast above dropped their copy
	// silently), and without this replay path the
	// creator's local roster would stay stale forever.
	// See internal/leavelog package doc for the full
	// design rationale.
	if n.leavelog != nil {
		if err := n.leavelog.Record(leavelog.Entry{
			GroupID: renderedID,
			LeftAt:  time.Now().UTC(),
		}); err != nil {
			log.Printf("[WARN ] leavelog: record %s: %v", renderedID, err)
		} else if err := n.leavelog.Save(); err != nil {
			// Log but don't fail the LeaveGroup — the
			// broadcast already did its best, and a
			// missing leavelog entry just means a future
			// restart won't replay (we'll catch the
			// creator up via the next in-person
			// conversation or via the user's manual
			// fix). Better to leave the group cleanly
			// than to surface an error to the GUI
			// after the user has already seen the
			// toast.
			log.Printf("[WARN ] leavelog: save %s: %v", renderedID, err)
		}
	}
	// v1.1 (2026-06-28): tell the GUI the group is gone
	// so it stops showing it in the sidebar. The frontend
	// also clears any selectedId that pointed at this
	// group (handled in leaveGroup()).
	n.publishGroupEvent(GroupEvent{
		Type:      GroupRemoved,
		GroupID:   renderedID,
		GroupName: m.GroupName,
	})
	return nil
}

// SendGroupFile broadcasts a file to every online member
// of the group (peer who currently has an active channel).
// It mirrors SendGroupMessage: one logical send, one fileID
// the caller pre-assigned (so the GUI can correlate the
// per-member transfers), and per-member fileIDs derived
// by appending "_<shortHex>" — each transfer runs as its
// own filetransfer.Send() call against the existing
// per-member channel.
//
// Per-member transfers are concurrent (filetransfer.Send
// spawns its own goroutine via pkg/node.SendFile). We open
// the file once per member rather than share a single
// reader: sharing would serialize 20 concurrent streams
// against one file handle, and at LAN scale 20 × open()
// is fine. Drop the offer to any member that has no
// active channel (offline = silent drop; outbox replay
// is Phase 5).
//
// Sender-side chat record: a single "file://<name>"
// bubble in the per-group chat.enc (not per-member —
// the GUI shows ONE bubble per logical file send, not N
// bubbles for N members). The chat record lands once
// regardless of how many members received the bytes.
//
// Returns the per-member fileIDs (one per online member
// that actually accepted the transfer) so the caller /
// GUI can track each transfer independently. The CLI
// doesn't care about this list today; the GUI does.
//
// v1.1 (2026-06-28).
func (n *Node) SendGroupFile(rawGroupID []byte, filePath, baseFileID string) ([]string, error) {
	if n.id == nil {
		return nil, errors.New("node: not started")
	}
	if filePath == "" {
		return nil, errors.New("node: SendGroupFile: empty path")
	}
	if baseFileID == "" {
		return nil, errors.New("node: SendGroupFile: empty baseFileID")
	}
	info, err := os.Stat(filePath)
	if err != nil {
		return nil, fmt.Errorf("node: SendGroupFile stat: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("node: SendGroupFile: not a regular file: %s", filePath)
	}
	size := info.Size()
	if size < 0 {
		return nil, errors.New("node: SendGroupFile: negative size")
	}
	rendered := group.RenderGroupID(rawGroupID)
	m, err := group.LoadMembers(n.dataDir(), rawGroupID)
	if err != nil {
		return nil, fmt.Errorf("node: SendGroupFile load members: %w", err)
	}
	selfHex := n.id.PeerIDHex()
	if !m.Contains(selfHex) {
		return nil, errors.New("node: SendGroupFile: sender is not a member")
	}
	name := info.Name()

	// Persist a single chat record for this send — one
	// bubble in our own group chat, regardless of how many
	// members received the file. Mirrors SendFile's per-peer
	// chat record semantics.
	now := time.Now().UTC()
	base := filepath.Base(name)
	sizeStr := humanSize(size)
	body := "file://" + base
	if sizeStr != "" {
		body += "|" + sizeStr
	}
	rec := &storage.Record{
		Timestamp: now,
		From:      selfHex,
		To:        "",
		Direction: "out",
		Body:      body,
		MsgID:     "",
		LocalPath: filePath,
		GroupID:   rendered,
	}
	if err := n.chatStore.AppendGroup(rendered, rec); err != nil {
		return nil, fmt.Errorf("node: SendGroupFile append chat: %w", err)
	}
	n.appendHistory(rec)
	n.publishMessage(Message{
		PeerID:    rendered,
		Body:      body,
		Timestamp: now,
		Direction: DirOut,
		LocalPath: filePath,
	})

	// Iterate online members and fan the file out.
	delivered := make([]string, 0, len(m.Members))
	for _, mem := range m.Members {
		if mem.PeerID == selfHex {
			continue
		}
		pid, err := hexToBytes(mem.PeerID)
		if err != nil {
			log.Printf("[WARN  ] SendGroupFile: bad member id %q: %v", mem.PeerID, err)
			continue
		}
		st := n.channels.get(pid)
		if st == nil {
			// Member offline — drop. Phase 5 outbox replay.
			continue
		}
		// Per-member fileID = base + "_" + first 8 chars of
		// member hex. The GUI sees each as a separate
		// file:event entry but the user-facing bubble remains
		// the single chat record we wrote above.
		memberShort := mem.PeerID
		if len(memberShort) > 8 {
			memberShort = memberShort[:8]
		}
		memberFileID := baseFileID + "_" + memberShort
		if err := n.sendGroupFileToOne(st.ch, filePath, name, size, memberFileID, rendered); err != nil {
			log.Printf("[WARN  ] SendGroupFile send to %s: %v", mem.PeerID, err)
			continue
		}
		delivered = append(delivered, memberFileID)
	}
	log.Printf("[GROUP ] sent file=%s to %d members (group=%s)", name, len(delivered), rendered)
	return delivered, nil
}

// sendGroupFileToOne opens filePath and streams one
// copy of it through ch via the existing per-member
// channel. The opened reader is bounded by the call's
// lifetime — filetransfer.Send returns after the offer
// is on the wire (it doesn't wait for chunks to drain),
// so by the time this function returns the file
// handle's refcount is up to 1 (the background sender
// goroutine). Caller must not close the underlying
// file until that goroutine has finished streaming —
// we transfer ownership via the io.Reader.
//
// In practice, opening a fresh *os.File per member is
// safe: the OS file handle lifetime is independent of
// this function returning, and the filetransfer sender
// reads until EOF. We use os.Open (not os.OpenFile)
// so the default perms match what SendFile does for
// 1:1 sends.
func (n *Node) sendGroupFileToOne(ch *protocol.Channel, filePath, name string, size int64, memberFileID, renderedGroupID string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()
	// Strip the "_<shortHex>" suffix to recover the base
	// fileID the GUI registered the placeholder with.
	// We emit FileEvent.FileID = baseFileID so the GUI's
	// existing state.fileBubbles lookup matches — one
	// bubble per logical send, not N per member.
	baseFileID := memberFileID
	if i := strings.LastIndex(memberFileID, "_"); i > 0 {
		baseFileID = memberFileID[:i]
	}
	// Sliding-window speed estimate (same approach as
	// SendFile — last 1s of progress samples, ~10 Hz
	// emit cadence). All per-member senders update the
	// SAME FileEvent.FileID = baseFileID, so the GUI
	// sees the LAST writer's progress. Imperfect but
	// works at LAN scale where all members' speeds are
	// similar; a future commit can do proper aggregation
	// (slowest-member as the bottleneck).
	var lastFlush time.Time
	var lastSent int64
	var windowBytes int64
	progressFn := func(sent, total int64) {
		now := time.Now()
		if lastFlush.IsZero() {
			lastFlush = now
			lastSent = sent
		}
		windowBytes += sent - lastSent
		lastSent = sent
		if now.Sub(lastFlush) < 100*time.Millisecond {
			return
		}
		bps := int64(float64(windowBytes) / now.Sub(lastFlush).Seconds())
		n.publishFileEvent(FileEvent{
			Type:        FileEventProgress,
			FileID:      baseFileID,
			Sent:        sent,
			Total:       total,
			BytesPerSec: bps,
			GroupID:     renderedGroupID,
		})
		lastFlush = now
		windowBytes = 0
	}
	if err := filetransfer.Send(n.ctx, ch, f, size, name, memberFileID, renderedGroupID, progressFn, nil); err != nil {
		// Surface the per-member failure to the GUI. The
		// bubble will get a 'done' event with ok=false; the
		// GUI's existing file:event 'done' handler shows
		// "失败: <err>" and a red bar. The baseFileID routing
		// means every member's failure overwrites the bubble
		// with that member's error string — again imperfect
		// but matches the v1.1 "one bubble, last write wins"
		// simplification. Aggregating per-member failure
		// counts would need a real coordinator goroutine.
		log.Printf("[WARN  ] sendGroupFileToOne: %v", err)
		n.publishFileEvent(FileEvent{
			Type:    FileEventDone,
			FileID:  baseFileID,
			Total:   size,
			OK:      false,
			Err:     err.Error(),
			GroupID: renderedGroupID,
		})
		return err
	}
	// Per-member success: emit a final progress tick +
	// a 'done' event. Note: if 3 members are streaming
	// concurrently, the GUI sees 3 'done' events in
	// quick succession; markFileBubbleDone is idempotent
	// (subsequent calls on an already-done bubble just
	// re-write the same state). This matches the
	// v1.1 "last-write-wins" simplification.
	n.publishFileEvent(FileEvent{
		Type:    FileEventProgress,
		FileID:  baseFileID,
		Sent:    size,
		Total:   size,
		GroupID: renderedGroupID,
	})
	n.publishFileEvent(FileEvent{
		Type:    FileEventDone,
		FileID:  baseFileID,
		Total:   size,
		OK:      true,
		GroupID: renderedGroupID,
	})
	return nil
}

	// humanSize is defined in messages.go (same package)
	// — we use the existing helper for the chat record's
	// "file://name|<size>" body so the GUI's bubble sees
	// identical formatting whether the send was 1:1 or
	// group.

// ctxWithNoCancel helper removed — SendGroupFile uses
// n.ctx directly so per-member transfers respect
// Node.Close() without needing a separate context.

// toGroupInfo converts pkg/group.Members → public GroupInfo.
// Reads the Aliases fresh from the alias store so the GUI
// shows current names (not stale ones in members.json).
func (n *Node) toGroupInfo(m *group.Members, rawGroupID []byte, isSelfMember bool) *GroupInfo {
	memberStrs := make([]string, 0, len(m.Members))
	for _, mem := range m.Members {
		memberStrs = append(memberStrs, mem.PeerID)
	}
	sort.Strings(memberStrs)
	return &GroupInfo{
		GroupID:   m.GroupID,
		GroupName: m.GroupName,
		Creator:   m.Creator,
		CreatedAt: m.CreatedAt,
		Members:   memberStrs,
		Self:      isSelfMember,
	}
}

// lookupPeerPublicKey returns the 64-byte SM2 public key for
// a peer we have an active channel with. The key was captured
// during the handshake and lives in channelState.pubKey.
// Returns an error if there's no active channel with that peer
// (group invites are 1:1 between currently-online peers — no
// channel = no invite possible).
func (n *Node) lookupPeerPublicKey(peerHex string) ([]byte, error) {
	pid, err := hexToBytes(peerHex)
	if err != nil {
		return nil, err
	}
	st := n.channels.get(pid)
	if st == nil {
		return nil, errors.New("node: no active channel with peer " + peerHex)
	}
	if len(st.pubKey) == 0 {
		return nil, errors.New("node: channel has empty pubKey (handshake didn't capture it)")
	}
	return st.pubKey, nil
}

// strings_TrimSpace is a tiny local helper to avoid pulling
// "strings" into this file just for TrimSpace.
func strings_TrimSpace(s string) string {
	start := 0
	end := len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}

// Avoid unused-import error for filepath — keep import even
// though we use it indirectly via dataDir.
var _ = filepath.Join

// sm2Unmarshal unpacks a 64-byte SM2 public key (X || Y big-
// endian, no prefix byte) into the *sm2.PublicKey gmsm type
// that pkg/group.Verify expects. Wraps crypto.SM2UnmarshalPublic
// so the dispatcher code stays short.
func sm2Unmarshal(b []byte) (*ic.SM2PublicKey, error) {
	return ic.SM2UnmarshalPublic(b)
}

// peerBytesToHex returns the lowercase-hex of a 16-byte
// PeerID, with a length guard. Used by the group dispatcher
// when we need to log which peer sent a message.
func peerBytesToHex(b []byte) string {
	if len(b) != 16 {
		return fmt.Sprintf("bad-peer-id-%dB", len(b))
	}
	return hex.EncodeToString(b)
}

// rosterPayload is the JSON shape of a TypeGroupRosterUpdate
// envelope's Payload. We send the entire current Members
// (group_id, group_name, creator, members) so the receiver
// can replace its local members.json wholesale. v1.1.1
// (2026-06-29).
//
// v1.1.2 (2026-06-30) hotfix: added Creator field.
// Without it, ApplyRosterUpdate on the receiver was
// forced to wipe the local Creator string (the inbound
// payload didn't carry it, the local kept-existing
// fallback wasn't there), which made every receiver's
// `g.creator === selfHex` check return false the next
// time ListGroups ran. UI symptom: after ANY peer joins
// / leaves / set-name / set-remark, the creator's own
// "+ 邀请成员" button + 群名 / 公告 编辑 disable + hints
// all disappear until restart. New binaries carry Creator
// forward; old binaries with no Creator in payload still
// receive a roster update but rely on the local-preserve
// fallback in ApplyRosterUpdate to keep creator status.
type rosterPayload struct {
	GroupID   string         `json:"group_id"`
	GroupName string         `json:"group_name"`
	Creator   string         `json:"creator"`
	Members   []group.Member `json:"members"`
	Remark    string         `json:"remark,omitempty"`
}

// metaPayload is the JSON shape of a TypeGroupMetaUpdate
// envelope's Payload. Carries just the editable fields
// (name, remark). Receivers update their local
// members.json in place. v1.1.1 (2026-06-29).
type metaPayload struct {
	GroupID   string `json:"group_id"`
	GroupName string `json:"group_name"`
	Remark    string `json:"remark,omitempty"`
}

// broadcastRosterUpdate sends the current roster to every
// member in m.Members. Best-effort: a failed send to one
// peer doesn't fail the whole broadcast — that peer just
// stays stale until the next roster-changing event.
//
// v1.1.1 (2026-06-29) hotfix: this used to take an
// `excludePeerID` parameter that skipped the joiner (the
// peer that just sent TypeGroupInviteAccept). That was
// wrong — the joiner only knows [creator, self] from
// AcceptGroupInvite, not the full roster, so excluding
// them left them permanently out of date about any
// OTHER members. Removed the parameter: every member
// (including the joiner) needs the roster so their
// local members.json matches the canonical one.
// Best-effort / idempotent — receiving a roster update
// that's identical to your local state is a no-op.
func (n *Node) broadcastRosterUpdate(m *group.Members) {
	if n.channels == nil {
		return
	}
	rendered := m.GroupID
	rawID, err := group.ParseGroupID(rendered)
	if err != nil {
		log.Printf("[WARN  ] broadcastRosterUpdate: bad group_id %q: %v", rendered, err)
		return
	}
	payload, err := json.Marshal(rosterPayload{
		GroupID:   m.GroupID,
		GroupName: m.GroupName,
		Creator:   m.Creator,
		Members:   m.Members,
		Remark:    m.Remark,
	})
	if err != nil {
		log.Printf("[WARN  ] broadcastRosterUpdate: marshal: %v", err)
		return
	}
	delivered := 0
	for _, mem := range m.Members {
		if n.id != nil && mem.PeerID == n.id.PeerIDHex() {
			// Skip self — self just wrote the same data
			// to its own members.json in the caller's
			// Save() right before calling us. The
			// GroupUpdated event fired alongside takes
			// care of refreshing the local frontend.
			continue
		}
		pid, err := hexToBytes(mem.PeerID)
		if err != nil {
			continue
		}
		st := n.channels.get(pid)
		if st == nil || st.ch == nil {
			// Peer offline — drop this copy. They'll pick
			// up the new state when they next reconnect
			// (a future re-sync API, or via the next
			// membership event after they rejoin).
			//
			// The st.ch == nil case is the integration
			// test harness: peer pubkey was seeded but no
			// real channel exists, so we drop the outbound
			// broadcast and let the harness drive the
			// receiver-side ApplyRosterUpdate directly.
			continue
		}
		env := protocol.Envelope{
			Type:    protocol.TypeGroupRosterUpdate,
			Payload: payload,
			GroupID: rawID,
		}
		if err := st.ch.Send(n.ctx, env); err != nil {
			log.Printf("[WARN  ] broadcastRosterUpdate to %s: %v", mem.PeerID, err)
			continue
		}
		delivered++
	}
	log.Printf("[GROUP ] roster update broadcast to %d/%d members",
		delivered, len(m.Members)-1)
}

// syncRostersToPeer pushes fresh TypeGroupRosterUpdate
// envelopes for every local group that includes peerHex
// in its roster. Best-effort / silent skip when peerHex
// is offline or in 0 groups.
//
// v1.1.2 (2026-06-30) hotfix — addresses the user-reported
// "stale member list after peer comes back online" symptom:
//
//   - Round 1: a peer is added to a group while peers A
//     and B are online. CreatorOnAccept broadcasts the
//     roster to both — they sync.
//   - Then B restarts their innerlink.exe. B's channels map
//     is empty. A's `n.channels.get(B)` is populated when
//     B's TCP handshake completes (line ~676 of node.go).
//   - But there's NO roster push from A's perspective — A's
//     local hasn't changed, so no broadcast fires.
//   - Result: B's local members.json is whatever it was at
//     B's last refresh point — could be stale (only
//     [creator, self]) even though it's now a member of a
//     3-person group.
//
// syncRostersToPeer closes that gap: as soon as the new
// channel is up, walk all groups where peerHex is a
// member, push fresh rosters over this channel. The
// peer's ApplyRosterUpdate rebuilds their local
// members.json and publishes GroupUpdated locally so the
// sidebar count re-aligns.
//
// We deliberately skip groups where peerHex is NOT a
// member — that would create a phantom members.json on
// their disk via ApplyRosterUpdate's "always Save"
// behavior, surfacing the group in their sidebar even
// though they were never invited. The check is
// `m.Contains(peerHex)` so this skips non-members
// silently.
func (n *Node) syncRostersToPeer(peerHex string, ch *protocol.Channel) {
	if n.id == nil || n.chatStore == nil || len(peerHex) == 0 || ch == nil {
		return
	}
	renderedIDs, err := n.chatStore.ListGroups()
	if err != nil {
		log.Printf("[WARN ] syncRostersToPeer(%s): list groups: %v", peerHex, err)
		return
	}
	sent := 0
	for _, rendered := range renderedIDs {
		rawID, err := group.ParseGroupID(rendered)
		if err != nil {
			continue
		}
		m, err := group.LoadMembers(n.dataDir(), rawID)
		if err != nil {
			continue
		}
		if !m.Contains(peerHex) {
			continue
		}
		payload, err := json.Marshal(rosterPayload{
			GroupID:   m.GroupID,
			GroupName: m.GroupName,
			Creator:   m.Creator,
			Members:   m.Members,
			Remark:    m.Remark,
		})
		if err != nil {
			log.Printf("[WARN ] syncRostersToPeer(%s): marshal %s: %v", peerHex, rendered, err)
			continue
		}
		env := protocol.Envelope{
			Type:    protocol.TypeGroupRosterUpdate,
			Payload: payload,
			GroupID: rawID,
		}
		if err := ch.Send(n.ctx, env); err != nil {
			log.Printf("[WARN ] syncRostersToPeer(%s): send %s: %v", peerHex, rendered, err)
			continue
		}
		sent++
	}
	if sent > 0 {
		log.Printf("[GROUP ] roster pre-sync to %s: %d group(s)", peerHex, sent)
	}
}

// broadcastMetaUpdate sends an updated name + remark to
// every member (except self). Best-effort like
// broadcastRosterUpdate. v1.1.1 (2026-06-29).
func (n *Node) broadcastMetaUpdate(m *group.Members) {
	if n.channels == nil {
		return
	}
	rendered := m.GroupID
	rawID, err := group.ParseGroupID(rendered)
	if err != nil {
		log.Printf("[WARN  ] broadcastMetaUpdate: bad group_id %q: %v", rendered, err)
		return
	}
	payload, err := json.Marshal(metaPayload{
		GroupID:   m.GroupID,
		GroupName: m.GroupName,
		Remark:    m.Remark,
	})
	if err != nil {
		log.Printf("[WARN  ] broadcastMetaUpdate: marshal: %v", err)
		return
	}
	delivered := 0
	for _, mem := range m.Members {
		if n.id != nil && mem.PeerID == n.id.PeerIDHex() {
			continue
		}
		pid, err := hexToBytes(mem.PeerID)
		if err != nil {
			continue
		}
		st := n.channels.get(pid)
		if st == nil {
			continue
		}
		env := protocol.Envelope{
			Type:    protocol.TypeGroupMetaUpdate,
			Payload: payload,
			GroupID: rawID,
		}
		if err := st.ch.Send(n.ctx, env); err != nil {
			log.Printf("[WARN  ] broadcastMetaUpdate to %s: %v", mem.PeerID, err)
			continue
		}
		delivered++
	}
	log.Printf("[GROUP ] meta update broadcast to %d members", delivered)
}

// ApplyRosterUpdate is the dispatcher-side handler for
// TypeGroupRosterUpdate. We treat the inbound roster as
// authoritative (the sender is the creator / roster-sync
// authority) and replace our local members.json with it.
// Note: we do NOT verify a signature here — the sender
// already auths via the encrypted channel (every channel
// has 1:1 SM4 keys from the handshake). A malicious
// creator could push a false roster, but that's the same
// threat as the creator being malicious in any other way
// (they can also broadcast false group messages), and
// the trust model in v1.1 is "trust the creator".
// v1.1.1 (2026-06-29).
func (n *Node) ApplyRosterUpdate(env protocol.Envelope, fromPeerID []byte) error {
	var rp rosterPayload
	if err := json.Unmarshal(env.Payload, &rp); err != nil {
		return fmt.Errorf("node: ApplyRosterUpdate unmarshal: %w", err)
	}
	rawID, err := group.ParseGroupID(rp.GroupID)
	if err != nil {
		return fmt.Errorf("node: ApplyRosterUpdate bad GroupID: %w", err)
	}
	// v1.1.4 (2026-07-02) hotfix: refuse to (re-)create
	// a group on our disk that we've already left.
	//
	// Scenario the user hit (2026-07-02 19:45 bug):
	//   1. A leaves g_xxx while creator C is offline.
	//      A's best-effort broadcast fails. A persists
	//      the leave in leavelog.
	//   2. C comes back online. C still has the stale
	//      3-member roster (A's leave was never delivered
	//      to C).
	//   3. A restarts. A's local groups/g_xxx/ is gone
	//      (LeaveGroup wiped it). On handshake, C's
	//      syncRostersToPeer pushes the 3-member roster
	//      back to A.
	//   4. Pre-fix, ApplyRosterUpdate blindly wrote
	//      members.json to A's disk, effectively
	//      un-leaving A against A's will. The user
	//      reported "另外两个重新打开程序, 群是没有的"
	//      but the log shows the group was being
	//      silently re-pulled on every reconnect.
	//
	// The fix: if A's leavelog contains rp.GroupID,
	// drop the inbound roster. A is explicitly NOT in
	// this group anymore; re-creating its on-disk state
	// would surface the group in A's sidebar against
	// A's intent. The leavelog will keep the notice
	// "stuck" until C applies it (ApplyLeaveNotice on
	// the next handshake), at which point C's roster
	// will no longer contain A and the conflict
	// resolves naturally.
	if n.leavelog != nil && n.leavelog.Contains(rp.GroupID) {
		log.Printf("[GROUP ] roster update from %s skipped: group=%s is in our leavelog (we already left)",
			peerBytesToHex(fromPeerID), rp.GroupID[:8])
		return nil
	}

	// v1.1.2 (2026-06-30) hotfix: preserve the local
	// Creator when the inbound roster update doesn't
	// carry one. Pre-fix, this branch always wrote
	// `Creator: ""`, which made every receiving peer's
	// ListGroups return GroupInfo.creator == "" — the
	// frontend's `g.creator === selfHex` test then
	// failed for the creator too, hiding the entire
	// creator-only UI surface ("+ 邀请成员" button, the
	// editable 群名称 + 群备注 inputs) until restart.
	//
	// Resolution: the new rosterPayload DOES carry
	// Creator (since v1.1.2) so the common case is
	// straightforward. The fallback below handles two
	// edge cases:
	//   - we have a local members.json already
	//     (AcceptGroupInvite wrote it), and the inbound
	//     payload omits Creator (older broadcast binary
	//     pre-v1.1.2 didn't add the field): preserve
	//     local Creator.
	//   - we DON'T have local members.json (rare;
	//     AcceptGroupInvite hasn't finished yet when a
	//     concurrent update arrives): fall back to
	//     whatever the inbound said. Most often that's
	//     also "" in this scenario — next reconnection
	//     / refresh from any peer will heal it.
	finalCreator := rp.Creator
	if finalCreator == "" {
		if existing, err := group.LoadMembers(n.dataDir(), rawID); err == nil && existing.Creator != "" {
			finalCreator = existing.Creator
		}
	}

	// We may not have a local members.json yet if the
	// roster update arrives before our AcceptGroupInvite
	// has finished its initial save. That's OK: write
	// the inbound roster as the canonical local state.
	m := &group.Members{
		GroupID:   rp.GroupID,
		GroupName: rp.GroupName,
		Creator:   finalCreator,
		// CreatedAt unknown on the receiver (sender
		// doesn't echo it). Leave zero — UI doesn't
		// surface it for non-creator members anyway.
		CreatedAt: time.Time{},
		Members:   rp.Members,
		Remark:    rp.Remark,
	}
	if err := m.Save(n.dataDir(), rawID); err != nil {
		return fmt.Errorf("node: ApplyRosterUpdate save: %w", err)
	}
	creatorShort := finalCreator
	if len(creatorShort) > 8 {
		creatorShort = creatorShort[:8]
	}
	log.Printf("[GROUP ] roster synced from %s: %d members (creator=%s)",
		peerBytesToHex(fromPeerID), len(rp.Members), creatorShort)
	// v1.1.1 (2026-06-29): tell the local frontend the
	// roster changed so the sidebar's "<N> 成员" count
	// and the settings panel's member list refresh
	// without the user closing + reopening anything.
	n.publishGroupEvent(GroupEvent{
		Type:      GroupUpdated,
		GroupID:   m.GroupID,
		GroupName: m.GroupName,
	})
	return nil
}

// ApplyMetaUpdate is the dispatcher-side handler for
// TypeGroupMetaUpdate. Updates the editable name + remark
// in our local members.json. v1.1.1 (2026-06-29).
func (n *Node) ApplyMetaUpdate(env protocol.Envelope, fromPeerID []byte) error {
	var mp metaPayload
	if err := json.Unmarshal(env.Payload, &mp); err != nil {
		return fmt.Errorf("node: ApplyMetaUpdate unmarshal: %w", err)
	}
	rawID, err := group.ParseGroupID(mp.GroupID)
	if err != nil {
		return fmt.Errorf("node: ApplyMetaUpdate bad GroupID: %w", err)
	}
	m, err := group.LoadMembers(n.dataDir(), rawID)
	if err != nil {
		// No local roster yet — can't apply. Caller
		// can re-fetch via ListGroups/GetGroup later.
		return fmt.Errorf("node: ApplyMetaUpdate load: %w", err)
	}
	if mp.GroupName != "" {
		m.GroupName = mp.GroupName
	}
	m.Remark = mp.Remark
	if err := m.Save(n.dataDir(), rawID); err != nil {
		return fmt.Errorf("node: ApplyMetaUpdate save: %w", err)
	}
	log.Printf("[GROUP ] meta synced from %s: name=%q remark_len=%d",
		peerBytesToHex(fromPeerID), m.GroupName, len(m.Remark))
	// v1.1.1 (2026-06-29): same as ApplyRosterUpdate —
	// let the local frontend know the metadata changed
	// so the sidebar name + settings panel refresh.
	n.publishGroupEvent(GroupEvent{
		Type:      GroupUpdated,
		GroupID:   m.GroupID,
		GroupName: m.GroupName,
	})
	return nil
}

// SetGroupName updates a group's display name and broadcasts
// the change to every other member. Returns the new GroupInfo
// on success. Caller must be the creator (v1.1.1 doesn't yet
// have a per-member rename-permission model — only the
// creator can change the name). v1.1.1 (2026-06-29).
func (n *Node) SetGroupName(renderedID, name string) (*GroupInfo, error) {
	if n.id == nil {
		return nil, errors.New("node: not started")
	}
	name = strings_TrimSpace(name)
	if name == "" {
		return nil, errors.New("node: SetGroupName: name is empty")
	}
	if len(name) > 30 {
		return nil, errors.New("node: SetGroupName: name too long (max 30 chars)")
	}
	rawID, err := group.ParseGroupID(renderedID)
	if err != nil {
		return nil, fmt.Errorf("node: SetGroupName bad GroupID: %w", err)
	}
	m, err := group.LoadMembers(n.dataDir(), rawID)
	if err != nil {
		return nil, fmt.Errorf("node: SetGroupName load: %w", err)
	}
	selfHex := n.id.PeerIDHex()
	if m.Creator != selfHex {
		return nil, errors.New("node: SetGroupName: only the creator can rename the group")
	}
	m.GroupName = name
	if err := m.Save(n.dataDir(), rawID); err != nil {
		return nil, fmt.Errorf("node: SetGroupName save: %w", err)
	}
	n.broadcastMetaUpdate(m)
	// v1.1.1 (2026-06-29): also notify the LOCAL frontend
	// (the creator's own GUI) so the sidebar name + chat
	// header update without waiting for the next external
	// event. broadcastMetaUpdate doesn't include self for
	// the same reason broadcastRosterUpdate doesn't — self
	// already has the latest via the local Save above.
	n.publishGroupEvent(GroupEvent{
		Type:      GroupUpdated,
		GroupID:   m.GroupID,
		GroupName: m.GroupName,
	})
	log.Printf("[GROUP ] name updated to %q on %s", name, renderedID)
	return n.toGroupInfo(m, rawID, true), nil
}

// SetGroupRemark updates a group's editable remark / notice
// and broadcasts it to every other member. Same permissions
// as SetGroupName (creator-only for v1.1.1). v1.1.1 (2026-06-29).
func (n *Node) SetGroupRemark(renderedID, remark string) (*GroupInfo, error) {
	if n.id == nil {
		return nil, errors.New("node: not started")
	}
	if len(remark) > 500 {
		return nil, errors.New("node: SetGroupRemark: remark too long (max 500 chars)")
	}
	rawID, err := group.ParseGroupID(renderedID)
	if err != nil {
		return nil, fmt.Errorf("node: SetGroupRemark bad GroupID: %w", err)
	}
	m, err := group.LoadMembers(n.dataDir(), rawID)
	if err != nil {
		return nil, fmt.Errorf("node: SetGroupRemark load: %w", err)
	}
	selfHex := n.id.PeerIDHex()
	if m.Creator != selfHex {
		return nil, errors.New("node: SetGroupRemark: only the creator can change the remark")
	}
	m.Remark = remark
	if err := m.Save(n.dataDir(), rawID); err != nil {
		return nil, fmt.Errorf("node: SetGroupRemark save: %w", err)
	}
	n.broadcastMetaUpdate(m)
	// v1.1.1 (2026-06-29): same as SetGroupName — fire
	// GroupUpdated locally so the creator's own GUI
	// refreshes the settings panel + sidebar without
	// waiting for the broadcast envelope to round-trip.
	n.publishGroupEvent(GroupEvent{
		Type:      GroupUpdated,
		GroupID:   m.GroupID,
		GroupName: m.GroupName,
	})
	log.Printf("[GROUP ] remark updated (%d chars) on %s", len(remark), renderedID)
	return n.toGroupInfo(m, rawID, true), nil
}

// ListGroupMembers returns the full per-member detail (alias,
// joined_at, is_creator) for a group. Used by the settings
// panel to render the member list. Reads from local
// members.json (which is kept in sync via TypeGroupRosterUpdate
// for non-creator peers). v1.1.1 (2026-06-29).
func (n *Node) ListGroupMembers(renderedID string) ([]GroupMemberDetail, error) {
	rawID, err := group.ParseGroupID(renderedID)
	if err != nil {
		return nil, fmt.Errorf("node: ListGroupMembers bad GroupID: %w", err)
	}
	m, err := group.LoadMembers(n.dataDir(), rawID)
	if err != nil {
		return nil, fmt.Errorf("node: ListGroupMembers load: %w", err)
	}
	out := make([]GroupMemberDetail, 0, len(m.Members))
	for _, mem := range m.Members {
		// Best-effort alias lookup: try the roster (for
		// currently-discovered peers); fall back to the
		// alias baked into members.json at accept time
		// (often empty for the creator). The frontend
		// also cross-references state.peers so this is
		// just a hint.
		alias := mem.Alias
		if n.rosterStore != nil && alias == "" {
			if entry, err := n.rosterStore.Get(mem.PeerID); err == nil {
				alias = entry.Alias
			}
		}
		out = append(out, GroupMemberDetail{
			PeerID:    mem.PeerID,
			Alias:     alias,
			JoinedAt:  mem.JoinedAt,
			IsCreator: mem.IsCreator,
			Self:      n.id != nil && mem.PeerID == n.id.PeerIDHex(),
		})
	}
	return out, nil
}

// GroupMemberDetail is one row in the settings panel's
// member list. Exposed to the Wails frontend.
type GroupMemberDetail struct {
	PeerID    string    `json:"peer_id"`
	Alias     string    `json:"alias"`
	JoinedAt  time.Time `json:"joined_at"`
	IsCreator bool      `json:"is_creator"`
	Self      bool      `json:"self"`
}
