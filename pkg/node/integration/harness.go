// Package integration_test is a 3-Node in-process harness for
// multi-peer scenario testing without the TCP / discovery /
// handshake transport layers. The goal is to drive
// group-state mutation across three Node instances and
// verify they converge to the same observable state —
// catching the kind of "two peers see different members in
// the same group" bugs that keep surfacing when we test
// manually.
//
// Why in-process (vs. launching 3 innerlink.exe processes):
//   - No port conflicts (each Node already has its own
//     DataDir + lockfile).
//   - Tests run fast (sub-second per scenario).
//   - We can drive individual envelope handlers (ApplyRoster
//     Update, ApplyLeaveNotice, AcceptGroupInvite, etc.)
//     directly with synthesized payloads — no need to wire
//     real transport sessions between Nodes.
//
// Why a separate package (vs. more _test.go in pkg/node):
//   - Integration tests should be able to fail without
//     breaking unit-test coverage of the same file.
//   - Keeps the action DSL out of the main test file.
//   - Lets us colocate scenario fuzzers in one place
//     (scenarios_test.go).
//
// Architecture note: the harness doesn't try to spin up
// real cross-Node channels. Instead, after each action we
// manually fan out the resulting envelope to every Node
// whose handler should see it. This is "what the network
// would do if every broadcast was reliable", which is the
// right model for state-consistency assertions — by
// definition the system must converge under reliable
// delivery. We add explicit disconnect / reconnect
// primitives (close + re-New one Node) for the offline
// case the user has been hitting.
package integration_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/weishengsuptp/innerlink/internal/protocol"
	"github.com/weishengsuptp/innerlink/pkg/group"
	"github.com/weishengsuptp/innerlink/pkg/node"
)

// Peer is one Node participating in a scenario. Each peer
// has its own DataDir (and thus its own lockfile + device.key
// + roster + chat.enc + groups/). DrainEventChannels must be
// running for Subscribe* events to land in peer.eventsPeers
// / eventsGroups / eventsMsgs — otherwise the eventsX
// slices stay empty and the test can't observe
// outbound-side state changes.
type Peer struct {
	Name    string         // human label, e.g. "alice"
	DataDir string         // per-peer DataDir on disk
	Node    *node.Node     // the Node under test
	Ctx     context.Context // long-lived ctx for the lifetime of this peer

	// Lifecycle bits. closed flips true after Close(); a
	// subsequent Restart() creates a fresh Node over
	// the same DataDir.
	closed bool

	// offline models "this peer is unreachable" for
	// in-process scenario tests. When set, every helper
	// that would push a roster / leave notice / message
	// to this peer (PushRosterFromTo, LeaveGroupAction,
	// AcceptInviteAction's broadcast step) skips it.
	//
	// Why we model offline this way: the harness is
	// in-process with no transport, so a real "process
	// gone" can't happen. But the production system
	// does the same skip — if a peer is unreachable,
	// roster updates and leave notices buffer on the
	// wire until reconnect. Skipping the push to
	// "offline" peers simulates the buffered state.
	muOffline sync.RWMutex
	offline   bool
}

// SetOffline toggles the offline flag. While offline,
// action helpers skip pushing roster / leave notices to
// this peer (see Peer.offline field comment).
func (p *Peer) SetOffline(v bool) {
	p.muOffline.Lock()
	p.offline = v
	p.muOffline.Unlock()
}

// IsOffline reports the current offline flag.
func (p *Peer) IsOffline() bool {
	p.muOffline.RLock()
	defer p.muOffline.RUnlock()
	return p.offline
}

// PeerID returns the 32-char hex PeerID of this Node, or "" if
// the Node was closed (device.key was released).
func (p *Peer) PeerID() string {
	if p.Node == nil {
		return ""
	}
	return p.Node.SelfPeerID()
}

// PeerIDBytes returns the raw 16-byte PeerID for callers that
// need to wire it into envelope fromPeerID slots.
func (p *Peer) PeerIDBytes() []byte {
	id, _ := hex.DecodeString(p.PeerID())
	return id
}

// Close shuts down the Node and releases its DataDir lock.
// Calling Close twice is a no-op (matching Node.Close's
// idempotent semantics).
func (p *Peer) Close() error {
	if p.closed || p.Node == nil {
		return nil
	}
	err := p.Node.Close()
	p.closed = true
	p.Node = nil
	return err
}

// Restart closes the Peer (if not already) and re-Nodes it
// against the same DataDir. Simulates a process exit + relaunch,
// which is the operation the user has been hitting in their
// manual tests. Each restart gets a new Lockfile (the previous
// one was released by Close), but device.key persists so
// the PeerID is stable (matches production semantics).
func (p *Peer) Restart() error {
	if err := p.Close(); err != nil {
		return fmt.Errorf("restart %s: pre-close: %w", p.Name, err)
	}
	newNode, err := node.New(node.Options{
		DataDir: p.DataDir,
		LogFile: filepath.Join(p.DataDir, "innerlink.log"),
		LogLevel: "error",
	})
	if err != nil {
		return fmt.Errorf("restart %s: re-New: %w", p.Name, err)
	}
	p.Node = newNode
	p.closed = false
	return nil
}

// Harness is the orchestrator. It owns the 3 Peers, runs an
// event-drain goroutine per peer, and provides the action
// primitives that operate on individual peers.
type Harness struct {
	t      *testing.T
	mu     sync.Mutex // guards peers slice access during Restart
	peers  []*Peer
	events map[string]peerEvents // peer name -> drained events
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// peerEvents aggregates the buffered events drained from a
// single Node's Subscribe* channels. We snapshot rather
// than replay because the test asserts on the latest state
// in the slice (the order they arrived in is also preserved).
type peerEvents struct {
	Peers  []node.PeerEvent
	Groups []node.GroupEvent
	Msgs   []node.Message
}

// NewHarness spins up `len(names)` peers, each with its
// own DataDir under t.TempDir(). Each peer gets a
// Subscribe*-drainer goroutine running until harness
// teardown via CloseAll(). t.Cleanup registers CloseAll so
// the test framework unwinds cleanly.
func NewHarness(t *testing.T, names []string) *Harness {
	if len(names) < 1 {
		t.Fatalf("harness needs at least 1 peer (got %d: %v)", len(names), names)
	}
	h := &Harness{
		t:      t,
		events: make(map[string]peerEvents),
	}
	h.ctx, h.cancel = context.WithCancel(context.Background())

	baseTmp := t.TempDir()
	for _, name := range names {
		dataDir := filepath.Join(baseTmp, name)
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dataDir, err)
		}
		n, err := node.New(node.Options{
			DataDir: dataDir,
			LogFile: filepath.Join(dataDir, "innerlink.log"),
			LogLevel: "error",
		})
		if err != nil {
			t.Fatalf("peer %s New: %v", name, err)
		}
		ctx, cancel := context.WithCancel(h.ctx)
		p := &Peer{Name: name, DataDir: dataDir, Node: n, Ctx: ctx}
		h.peers = append(h.peers, p)
		h.events[name] = peerEvents{}
		h.startDrainer(name, p)
		// not used right now but keeps cancel reachable
		_ = cancel
	}

	// Register every peer's public key with every other
	// peer. Production populates this via the handshake;
	// the harness has no handshake so we seed it
	// directly. Without this step, AcceptGroupInvite
	// fails with "no active channel with peer X" because
	// lookupPeerPublicKey walks channelState.
	//
	// Loop order: for each "self" peer p, register every
	// "other" peer q's pubkey under q's peerID, in p's
	// channel-state map.
	for _, p := range h.peers {
		for _, q := range h.peers {
			if p.Node.SelfPeerID() == q.Node.SelfPeerID() {
				continue
			}
			if err := p.Node.RegisterPeerPublicKeyForTest(q.PeerID(), q.Node.SelfPublicKey()); err != nil {
				t.Fatalf("register pubkey %s→%s: %v", p.Name, q.Name, err)
			}
		}
	}
	t.Cleanup(func() {
		h.CloseAll()
	})
	return h
}

// startDrainer spawns a goroutine that copies a peer's
// Subscribe* events into the harness's per-peer events
// slice. The drainers are cancelled by CloseAll().
func (h *Harness) startDrainer(name string, p *Peer) {
	h.wg.Add(3)
	go func() {
		defer h.wg.Done()
		ch := p.Node.SubscribePeers()
		for {
			select {
			case <-h.ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				h.mu.Lock()
				e := h.events[name]
				e.Peers = append(e.Peers, ev)
				h.events[name] = e
				h.mu.Unlock()
			}
		}
	}()
	go func() {
		defer h.wg.Done()
		ch := p.Node.SubscribeGroups()
		for {
			select {
			case <-h.ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				h.mu.Lock()
				e := h.events[name]
				e.Groups = append(e.Groups, ev)
				h.events[name] = e
				h.mu.Unlock()
			}
		}
	}()
	go func() {
		defer h.wg.Done()
		ch := p.Node.SubscribeMessages()
		for {
			select {
			case <-h.ctx.Done():
				return
			case ev, ok := <-ch:
				if !ok {
					return
				}
				h.mu.Lock()
				e := h.events[name]
				e.Msgs = append(e.Msgs, ev)
				h.events[name] = e
				h.mu.Unlock()
			}
		}
	}()
}

// CloseAll stops the drainers and closes every peer Node.
// Safe to call multiple times. Returns the first non-nil
// error from the per-peer Closes.
func (h *Harness) CloseAll() error {
	if h.cancel != nil {
		h.cancel()
		h.cancel = nil
	}
	h.wg.Wait()
	h.mu.Lock()
	peers := h.peers
	h.peers = nil
	h.mu.Unlock()
	var firstErr error
	for _, p := range peers {
		if err := p.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Peer returns the peer with the given name, failing the test
// if no such peer exists. Names are matched exactly.
func (h *Harness) Peer(name string) *Peer {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.peers {
		if p.Name == name {
			return p
		}
	}
	h.t.Fatalf("harness: no peer named %q", name)
	return nil
}

// peerByID returns the Peer whose PeerID() matches hexID,
// or nil if no match. Used to translate the
// "peerID bytes from members.json" world into a Peer
// reference for harness-side actions.
func (h *Harness) peerByID(hexID string) *Peer {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.peers {
		if p.Node != nil && p.Node.SelfPeerID() == hexID {
			return p
		}
	}
	return nil
}

// reRegisterPubkeys re-seeds every peer's pubkey cache
// for every other peer. Used after a Peer.Restart(),
// which clears the in-memory channel registry.
//
// Why this is needed: RegisterPeerPublicKeyForTest
// populates the in-memory channelState map, not the
// on-disk key file. After Close() the map is dropped.
// Restart() creates a fresh Node but the test's
// pubkey seeding is gone, so AcceptGroupInvite would
// fail with "no active channel with peer X" until the
// next seed.
//
// Production never has this problem because pubkeys
// come from the on-disk roster (the persistent
// channel-state pubkey lives in the device.key path,
// which survives restart). The harness doesn't have
// an on-disk equivalent, so we re-seed manually.
func (h *Harness) reRegisterPubkeys() {
	for _, p := range h.peers {
		if p.Node == nil {
			continue
		}
		for _, q := range h.peers {
			if p.Node.SelfPeerID() == q.Node.SelfPeerID() {
				continue
			}
			if q.Node == nil {
				continue
			}
			// Best-effort: ignore errors here, the
			// test will surface the real failure on
			// the next AcceptGroupInvite call.
			_ = p.Node.RegisterPeerPublicKeyForTest(q.PeerID(), q.Node.SelfPublicKey())
		}
	}
}

// Events returns a snapshot of one peer's drained events.
// Mutating the returned slice will not race the drainer
// because we copy under h.mu before returning.
func (h *Harness) Events(peerName string) peerEvents {
	h.mu.Lock()
	defer h.mu.Unlock()
	src := h.events[peerName]
	out := peerEvents{
		Peers:  append([]node.PeerEvent(nil), src.Peers...),
		Groups: append([]node.GroupEvent(nil), src.Groups...),
		Msgs:   append([]node.Message(nil), src.Msgs...),
	}
	return out
}

// Snapshot captures per-peer observable state. It's the
// thing the diff engine and consistency assertions operate
// on. Snapshot() is the only way the test sees across-peer
// state — handlers are not directly observable from outside.
type Snapshot struct {
	At      time.Time
	PerPeer map[string]*PeerSnapshot
}

// PeerSnapshot is everything we care about for one peer at
// one instant. We pull data once (no caching) so the
// snapshot reflects whatever the Node reports right now.
type PeerSnapshot struct {
	Name         string
	PeerID       string // hex peer ID of this peer
	Groups       []node.GroupInfo
	RosterByID   map[string]RosterEntry // peerID hex -> snapshot entry
	Leavelog     []LeavelogEntry         // own leavelog
	GroupDirs    map[string][]string    // rendered group ID -> peerID hex list
}

// RosterEntry is the JSON-friendly shape of a roster.Entry.
// We only pull fields the test asserts on (PeerID, Alias,
// Reset), not the full gossip metadata.
type RosterEntry struct {
	PeerID string
	Alias  string
	Reset  bool
	// Addr is the first non-loopback address we know for
	// this peer (for human-friendly diff output). Empty if
	// the peer hasn't reported any address yet.
	Addr string
}

// LeavelogEntry mirrors internal/leavelog.Entry so the test
// package doesn't need to import it (and so a snapshot
// stays JSON-serializable for fault reporting).
type LeavelogEntry struct {
	GroupID string
	LeftAt  time.Time
}

// Snapshot builds a Snapshot of the harness's current
// state. Errors from each peer's ListGroups are recorded in
// the PeerSnapshot.Errors field — they're test failures if
// non-empty (operational errors aren't expected under
// normal test conditions).
func (h *Harness) Snapshot() *Snapshot {
	h.mu.Lock()
	peers := append([]*Peer(nil), h.peers...)
	h.mu.Unlock()
	snap := &Snapshot{
		At:      time.Now(),
		PerPeer: make(map[string]*PeerSnapshot, len(peers)),
	}
	for _, p := range peers {
		ps := &PeerSnapshot{
			Name:       p.Name,
			PeerID:     p.PeerID(),
			RosterByID: map[string]RosterEntry{},
			GroupDirs:  map[string][]string{},
		}
		if p.Node == nil {
			// Peer is closed/restarting — record zero
			// state. Tests that try to read this state
			// and fail are correctly catching the gap.
			snap.PerPeer[p.Name] = ps
			continue
		}
		groups, err := p.Node.ListGroups()
		if err != nil {
			h.t.Errorf("peer %s ListGroups: %v", p.Name, err)
		} else {
			for _, g := range groups {
				ps.Groups = append(ps.Groups, *g)
				mems, err := p.Node.ListGroupMembers(g.GroupID)
				if err != nil {
					h.t.Errorf("peer %s ListGroupMembers(%s): %v",
						p.Name, shortGroupID(g.GroupID), err)
					continue
				}
				for _, mem := range mems {
					ps.GroupDirs[g.GroupID] = append(ps.GroupDirs[g.GroupID], mem.PeerID)
				}
			}
		}
		ps.Leavelog = h.readLeavelog(p)
		// Roster: pull via the Node's dataDir's roster
		// file. Direct read keeps the test out of the
		// Node's API surface and works even if SubscribePeers
		// hasn't drained yet.
		ps.RosterByID = h.readRoster(p)
		snap.PerPeer[p.Name] = ps
	}
	return snap
}

// readLeavelog reads DataDir/leaved_groups.json directly.
// Tolerates a missing file (returns empty slice, since the
// Store treats missing as empty).
func (h *Harness) readLeavelog(p *Peer) []LeavelogEntry {
	path := filepath.Join(p.DataDir, "leaved_groups.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return nil
	}
	// Mirror the on-disk schema (same v1 as
	// internal/leavelog.fileFormat). We re-decode here
	// rather than import the package because integration
	// tests shouldn't pull internal/* into the public
	// test surface — the JSON format is stable and is
	// documented at the file's writer.
	var f struct {
		V       int `json:"v"`
		Entries []struct {
			GroupID string    `json:"group_id"`
			LeftAt  time.Time `json:"left_at"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil
	}
	out := make([]LeavelogEntry, 0, len(f.Entries))
	for _, e := range f.Entries {
		out = append(out, LeavelogEntry{GroupID: e.GroupID, LeftAt: e.LeftAt})
	}
	return out
}

// readRoster reads DataDir/roster.json and indexes by PeerID.
// Tolerates missing file (empty roster).
func (h *Harness) readRoster(p *Peer) map[string]RosterEntry {
	path := filepath.Join(p.DataDir, "roster.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]RosterEntry{}
		}
		return map[string]RosterEntry{}
	}
	var f struct {
		V       int `json:"v"`
		Entries []struct {
			PeerID    string   `json:"peer_id"`
			Hostname  string   `json:"hostname,omitempty"`
			Alias     string   `json:"alias,omitempty"`
			Addrs     []string `json:"addrs,omitempty"`
			Reset     bool     `json:"reset,omitempty"`
			FirstSeen string   `json:"first_seen,omitempty"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return map[string]RosterEntry{}
	}
	out := make(map[string]RosterEntry, len(f.Entries))
	for _, e := range f.Entries {
		addr := ""
		if len(e.Addrs) > 0 {
			addr = e.Addrs[0]
		}
		out[e.PeerID] = RosterEntry{
			PeerID: e.PeerID,
			Alias:  e.Alias,
			Reset:  e.Reset,
			Addr:   addr,
		}
	}
	return out
}

// Helpers used by action implementations. These are the
// "synthesize an envelope and call the receiver-side
// handler" primitives that let us drive the broadcast
// paths without setting up real cross-Node channels.

// SynthesizeInviteEnvelope builds the envelope that
// InviteToGroup would have sent. Returned envelope is
// suitable for passing to AcceptGroupInvite.
func SynthesizeInviteEnvelope(p *Peer, inv *group.Invite) protocol.Envelope {
	rawID, _ := group.ParseGroupID(inv.GroupID)
	payload, _ := json.Marshal(inv)
	return protocol.Envelope{
		Type:    protocol.TypeGroupInvite,
		Payload: payload,
		GroupID: rawID,
	}
}

// SynthesizeAcceptEnvelope builds the envelope that
// AcceptGroupInvite would have sent back to the inviter.
// Suitable for passing to CreatorOnAccept.
//
// fromPeerID in the envelope From field is the invitee's
// peer ID bytes (since they're the sender); callers pass
// invitee.PeerIDBytes() to CreatorOnAccept as the
// fromPeerID arg.
func SynthesizeAcceptEnvelope(invitee *Peer, inv *group.Invite) protocol.Envelope {
	payload, _ := json.Marshal(map[string]interface{}{
		"group_id": inv.GroupID,
		"nonce":    inv.Nonce,
	})
	return protocol.Envelope{
		Type:    protocol.TypeGroupInviteAccept,
		Payload: payload,
	}
}

// SynthesizeMetaEnvelope builds a TypeGroupMetaUpdate
// envelope from the given peer (the broadcaster). The
// receiver-side ApplyMetaUpdate applies it to local
// members.json in place. Used by scenarios that
// exercise rename / remark without going through real
// channel broadcasts.
func SynthesizeMetaEnvelope(from *Peer, rawGroupID []byte, name, remark string) protocol.Envelope {
	payload, _ := json.Marshal(map[string]string{
		"group_id":   group.RenderGroupID(rawGroupID),
		"group_name": name,
		"remark":     remark,
	})
	return protocol.Envelope{
		Version: protocol.ProtocolVersion,
		Type:    protocol.TypeGroupMetaUpdate,
		From:    from.PeerIDBytes(),
		Payload: payload,
		GroupID: rawGroupID,
		TS:      time.Now().UnixMilli(),
	}
}

// SynthesizeRosterEnvelope builds the envelope that
// broadcastRosterUpdate would have sent. Used to push
// post-add roster state from one peer to another without
// going through the channel layer.
func SynthesizeRosterEnvelope(p *Peer, groupID string, m *group.Members) protocol.Envelope {
	rawID, _ := group.ParseGroupID(groupID)
	payload, _ := json.Marshal(rosterPayloadFromMembers(m))
	return protocol.Envelope{
		Type:    protocol.TypeGroupRosterUpdate,
		Payload: payload,
		GroupID: rawID,
	}
}

// PushRosterFromTo fakes the post-add roster push: reads
// `from`'s on-disk members.json for the group, then
// invokes ApplyRosterUpdate on every other peer with that
// roster. This is what broadcastRosterUpdate does in
// production except without a channel — and it lets the
// harness deterministically verify every receiver.
//
// Returns the number of receivers it pushed to.
func (h *Harness) PushRosterFromTo(fromName, groupID string, toNames []string) int {
	from := h.Peer(fromName)
	pushed := 0
	for _, toName := range toNames {
		to := h.Peer(toName)
		if from == nil || to == nil || from.Node == nil || to.Node == nil || fromName == toName {
			continue
		}
		// Skip offline peers — the production system
		// would buffer the roster until reconnect.
		if to.IsOffline() {
			continue
		}
		rawID, err := group.ParseGroupID(groupID)
		if err != nil {
			continue
		}
		m, err := group.LoadMembers(from.DataDir, rawID)
		if err != nil {
			continue
		}
		env := SynthesizeRosterEnvelope(from, groupID, m)
		if err := to.Node.ApplyRosterUpdate(env, from.PeerIDBytes()); err != nil {
			h.t.Errorf("PushRosterFromTo %s→%s ApplyRosterUpdate: %v", fromName, toName, err)
			continue
		}
		pushed++
	}
	return pushed
}

// rosterPayloadFromMembers mirrors pkg/node.rosterPayload.
// Both packages live in different modules so we duplicate
// the wire shape (it hasn't changed since v1.1 and is the
// canonical definition in pkg/node/groups.go).
type rosterPayloadJSON struct {
	GroupID   string         `json:"group_id"`
	GroupName string         `json:"group_name"`
	Creator   string         `json:"creator"`
	Members   []group.Member `json:"members"`
	Remark    string         `json:"remark,omitempty"`
}

func rosterPayloadFromMembers(m *group.Members) rosterPayloadJSON {
	return rosterPayloadJSON{
		GroupID:   m.GroupID,
		GroupName: m.GroupName,
		Creator:   m.Creator,
		Members:   m.Members,
		Remark:    m.Remark,
	}
}

// errUnused is used at package init so imports don't get
// cut when the helper functions grow.
var _ = errors.New
