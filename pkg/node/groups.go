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
	"sync"
	"time"

	ic "github.com/weishengsuptp/innerlink/internal/crypto"
	"github.com/weishengsuptp/innerlink/internal/filetransfer"
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

	// Build members.json — creator first, then invitees
	// (sorted by joinedAt to match the on-disk stability
	// invariant from pkg/group).
	members := []group.Member{
		{PeerID: selfHex, Alias: n.GetSelfAlias(), JoinedAt: now, IsCreator: true},
	}
	// Invitees get their own JoinedAt = creator's now (they
	// haven't actually joined yet — InviteToGroup will add
	// the canonical JoinedAt when they accept).
	for _, pid := range memberPeerIDs {
		if pid == "" || pid == selfHex {
			continue // skip self + empty
		}
		members = append(members, group.Member{PeerID: pid, JoinedAt: now})
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
	if n.id == nil {
		return nil, errors.New("node: not started")
	}
	rendered := group.RenderGroupID(rawGroupID)
	m, err := group.LoadMembers(n.dataDir(), rawGroupID)
	if err != nil {
		return nil, fmt.Errorf("node: InviteToGroup load: %w", err)
	}
	if inviteePeerID == "" {
		return nil, errors.New("node: InviteToGroup: invitee empty")
	}
	// The inviter MUST be a current member (we don't allow
	// random peers to spam invites for groups they don't
	// belong to).
	selfHex := n.id.PeerIDHex()
	if !m.Contains(selfHex) {
		return nil, errors.New("node: InviteToGroup: inviter is not a member of this group")
	}
	if m.Contains(inviteePeerID) {
		return nil, errors.New("node: InviteToGroup: invitee already a member")
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
		return nil, fmt.Errorf("node: InviteToGroup sign: %w", err)
	}
	inv.Signature = sig
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
	log.Printf("[GROUP ] accepted invite to %s (inviter=%s)", inv.GroupID, inviterHex)
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
	if st == nil {
		// Inviter went offline between invite and accept.
		// They can read our accept when they next connect
		// (the channel.Send path returns error here — we
		// log + skip; the inviter will see us as not-yet-
		// joined in their next manual scan).
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
	if st == nil {
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
func (n *Node) CreatorOnAccept(env protocol.Envelope, fromPeerID []byte) error {
	var ap acceptPayload
	if err := json.Unmarshal(env.Payload, &ap); err != nil {
		return fmt.Errorf("node: CreatorOnAccept unmarshal: %w", err)
	}
	rawID, err := group.ParseGroupID(ap.GroupID)
	if err != nil {
		return fmt.Errorf("node: CreatorOnAccept bad GroupID: %w", err)
	}
	m, err := group.LoadMembers(n.dataDir(), rawID)
	if err != nil {
		return fmt.Errorf("node: CreatorOnAccept load: %w", err)
	}
	accepterHex := peerBytesToHex(fromPeerID)
	if m.Contains(accepterHex) {
		log.Printf("[GROUP ] accept from %s: already member, no-op", accepterHex)
		return nil
	}
	if err := m.AddMember(group.Member{
		PeerID:   accepterHex,
		JoinedAt: time.Now().UTC(),
	}); err != nil {
		return fmt.Errorf("node: CreatorOnAccept add member: %w", err)
	}
	if err := m.Save(n.dataDir(), rawID); err != nil {
		return fmt.Errorf("node: CreatorOnAccept save: %w", err)
	}
	log.Printf("[GROUP ] %s accepted invite to %s; roster updated", accepterHex, ap.GroupID)
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

	// Build the envelope ONCE, send to every online member.
	// Per-member sends share the same envelope bytes — the
	// only thing that differs is the Channel (different
	// session keys per peer).
	env := protocol.Envelope{
		Type:    protocol.TypeText,
		GroupID: rawGroupID,
		Payload: []byte(text),
	}
	delivered := 0
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
		if st == nil {
			// Member offline — drop this copy. They'd
			// pick up the chat.enc via outbox replay
			// (phase 5). For v1.1 group, offline
			// messages are silently dropped.
			continue
		}
		if err := st.ch.Send(n.ctx, env); err != nil {
			log.Printf("[WARN  ] SendGroupMessage send to %s: %v", mem.PeerID, err)
			continue
		}
		delivered++
	}
	log.Printf("[GROUP ] sent to %s: %d/%d delivered (out)", rendered, delivered, len(m.Members)-1)
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
		out = append(out, Message{
			PeerID:    renderedID,
			Body:      r.Body,
			Timestamp: r.Timestamp,
			Direction: r.Direction,
			LocalPath: r.LocalPath,
		})
	}
	return out, nil
}

// LeaveGroup removes self from the group's roster and
// deletes the local directory (chat.enc + members.json).
// Other members see the leave via... a future TypeGroupLeave
// envelope; for v1.1 we just drop locally and the next
// message we send will implicitly re-add (TODO: send leave
// envelope).
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
	// Don't allow creator to leave (would orphan the group).
	if m.Creator == selfHex {
		return errors.New("node: LeaveGroup: creator cannot leave (dissolve group instead)")
	}
	if !m.RemoveMember(selfHex) {
		return errors.New("node: LeaveGroup: RemoveMember returned false")
	}
	// If the group is empty after our removal, delete it.
	// Otherwise save the updated roster.
	if len(m.Members) == 0 {
		return n.chatStore.DeleteGroup(renderedID)
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
	log.Printf("[GROUP ] left group=%s (local cleanup done)", renderedID)
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
	// skipChatLog=true because we already wrote the
	// chat record above (single-bubble-per-send
	// semantics). Per-member filetransfer.Send calls
	// into n.SendFile internally via... actually no,
	// they don't — SendGroupFile has its own per-member
	// loop and doesn't go through SendFile (SendFile
	// also writes a chat record, which we don't want
	// per-member). We use filetransfer.Send directly
	// here so we have full control.
	//
	// The fileID we pass is per-member so each
	// concurrent transfer can be tracked independently
	// by the GUI's file:event stream.
	progressFn := func(sent, total int64) {
		pct := int64(0)
		if total > 0 {
			pct = sent * 100 / total
		}
		log.Printf("[FILE] group sending %s to <member> %d/%d bytes (%d%%)",
			name, sent, total, pct)
	}
	// We use n.ctx so the transfer respects Close().
	// Per-file cancel isn't exposed in v1.1 (would
	// require a per-member CancelFileGroup binding).
	return filetransfer.Send(n.ctx, ch, f, size, name, memberFileID, renderedGroupID, progressFn, nil)
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
