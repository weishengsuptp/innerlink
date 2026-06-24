package node

import (
	"strings"
	"time"

	"github.com/weishengsuptp/innerlink/internal/alias"
	"github.com/weishengsuptp/innerlink/internal/identity"
	"github.com/weishengsuptp/innerlink/internal/roster"
)

// PeerInfo is the public, UI-facing view of one peer.
// It merges three sources of truth into a single struct:
//
//   - the alias table (M4: human-readable name + last-seen)
//   - the LAN roster (M5: hostname + addresses from gossip)
//   - the channel registry (live "are we connected right now?")
//
// Updated on every relevant event (alias touch, roster merge,
// channel ready / closed). The fields are stable: a UI
// listing of peers can re-poll ListPeers() at any time
// without worrying about consistency.
type PeerInfo struct {
	// PeerID is the 32-char lowercase hex SM2-derived ID.
	PeerID string

	// Name is the legacy per-peer local nickname from
	// internal/alias (the old "alias <name> <peer>"
	// REPL table). Kept for backward compatibility with
	// the legacy REPL commands; the GUI does NOT use
	// this field — see SelfAlias below.
	Name string

	// SelfAlias is the peer's broadcast self-display-name
	// (from <DataDir>/alias.txt on their side, propagated
	// via M5 RosterSync). "" if the peer hasn't set one
	// yet. For our own entry (IsSelf=true), this is our
	// own self-alias from our own alias.txt. The GUI
	// uses this field as the primary display name,
	// falling back to Hostname and then to a placeholder.
	SelfAlias string

	// Hostname is the remote machine's hostname as
	// announced via M5 gossip. "" if unknown.
	Hostname string

	// Addrs is the list of "ip:port" strings the peer has
	// announced (via roster sync + UDP discovery). The first
	// entry is the most recent. May be empty if the peer is
	// only known via alias touch but never seen on the wire.
	Addrs []string

	// LastSeen is the most recent activity timestamp
	// (alias touch, channel-ready, or roster merge).
	LastSeen time.Time

	// Online is true iff we currently have an active
	// encrypted Channel to this peer.
	Online bool

	// IsSelf is true iff this entry is our own device
	// (always present in the roster so we can publish
	// "how to reach me" on the first channel-ready).
	IsSelf bool

	// Reset is the dedup one-shot marker. When true,
	// this entry is the ghost of a previous install at
	// the same (IP, hostname) as a newer peerID. ListPeers
	// already filters Reset=true out (UI never sees it),
	// but the field is exposed for diagnostic / test
	// use. Production callers should treat any Reset=true
	// PeerInfo as a bug.
	Reset bool
}

// PeerEventType enumerates the kinds of transitions
// delivered on SubscribePeers.
type PeerEventType string

const (
	PeerAdded   PeerEventType = "added"   // discovery saw this peer for the first time
	PeerRemoved PeerEventType = "removed" // discovery timed out this peer
	PeerOnline  PeerEventType = "online"  // channel became ready
	PeerOffline PeerEventType = "offline" // channel closed
)

// PeerEvent is one transition. SubscribePeers() delivers
// these as they happen.
type PeerEvent struct {
	Type   PeerEventType
	PeerID string
	// Addr is set for PeerAdded only; empty for the others.
	Addr string
}

// SubscribePeers returns a channel of peer transitions.
// The channel is closed when Close() is called.
//
// Buffered to 64 events; drops oldest under sustained
// flood (the LAN directory converges on the next sync
// anyway, so missing one event is recoverable by
// re-polling ListPeers()).
func (n *Node) SubscribePeers() <-chan PeerEvent {
	return n.peerEventCh
}

// ListPeers returns the current view of every peer we
// know about. Combines the alias table (for names +
// last-seen) with the roster (for hostnames + addresses
// + broadcast self-aliases) and the channel registry
// (for live online state).
//
// Roster entries marked Reset=true (the dedup ghost of
// a previous install at the same IP+hostname) are
// filtered out here — the UI never sees them, even if
// the underlying roster.json still has them. The filter
// is sticky across restarts.
//
// Safe to call from any goroutine.
func (n *Node) ListPeers() []PeerInfo {
	// Build a map keyed by PeerID so we can merge the
	// three sources without losing entries that appear
	// in only one of them (e.g. alias-only with no
	// roster entry, or roster-only with no alias).
	out := make(map[string]*PeerInfo)

	// Source 1: alias table (gives Name + LastSeen for
	// any peer we've ever seen, including those we
	// haven't heard from in a while). The legacy
	// local-nickname system.
	for _, row := range n.aliasStore.ListWithNames() {
		info, ok := out[row.PeerID]
		if !ok {
			info = &PeerInfo{PeerID: row.PeerID}
			out[row.PeerID] = info
		}
		info.Name = row.Name
		info.LastSeen = row.LastSeen
	}

	// Source 2: roster (gives Hostname + SelfAlias +
	// Addrs). Use ListActive() so Reset=true ghosts are
	// filtered here, BEFORE we build PeerInfo, so the
	// dedup logic is invisible to the UI layer.
	for _, e := range n.rosterStore.ListActive() {
		info, ok := out[e.PeerID]
		if !ok {
			info = &PeerInfo{PeerID: e.PeerID}
			out[e.PeerID] = info
		}
		info.Hostname = e.Hostname
		info.SelfAlias = e.Alias
		info.Addrs = append([]string(nil), e.Addrs...)
	}

	// Source 3: channel registry (gives Online + isSelf
	// + RemoteAddr as a fallback for Addrs). The channel
	// registry already filters out Reset ghosts because
	// we never open a channel to one — but defensively
	// skip any Reset=true entry here too, in case the
	// channel registry ever changes that policy.
	selfHex := n.id.PeerIDHex()
	for _, st := range n.channels.snapshot() {
		ph := peerHex(st.peerID)
		if existing, ok := out[ph]; ok && existing.Reset {
			continue
		}
		info, ok := out[ph]
		if !ok {
			info = &PeerInfo{PeerID: ph}
			out[ph] = info
		}
		info.Online = true
		if info.LastSeen.IsZero() {
			info.LastSeen = time.Now()
		}
		if len(info.Addrs) == 0 && st.ch.RemoteAddr() != "" {
			info.Addrs = []string{st.ch.RemoteAddr()}
		}
	}

	// Mark self.
	if self, ok := out[selfHex]; ok {
		self.IsSelf = true
	} else {
		// Defensive: if for some reason self isn't in
		// the roster yet (race during construction),
		// synthesize it from identity.
		out[selfHex] = &PeerInfo{
			PeerID:    selfHex,
			IsSelf:    true,
			Hostname:  localHostname(),
			SelfAlias: n.selfAlias.Get(),
			Addrs:     []string{n.ann.LocalAddr()},
			LastSeen:  time.Now(),
		}
	}

	// Stable order: sort by LastSeen desc, with self pinned first.
	infos := make([]PeerInfo, 0, len(out))
	for _, v := range out {
		infos = append(infos, *v)
	}
	sortPeers(infos)
	return infos
}

func sortPeers(p []PeerInfo) {
	// Self first, then by LastSeen desc.
	// (A future UI may want online-first; we don't have
	// that signal cheaply for entries not in the channel
	// registry, so recency is the next-best proxy.)
	for i := 0; i < len(p); i++ {
		for j := i + 1; j < len(p); j++ {
			pi, pj := p[i], p[j]
			switch {
			case pi.IsSelf && !pj.IsSelf:
				// pi stays before pj
			case !pi.IsSelf && pj.IsSelf:
				p[i], p[j] = p[j], p[i]
			case pi.LastSeen.After(pj.LastSeen):
				p[i], p[j] = p[j], p[i]
			}
		}
	}
}

func localHostname() string {
	return hostname()
}

// keep imports referenced for future expansion.
var (
	_ = identity.PeerIDSize
	_ = roster.Entry{}
	_ = alias.Open
	_ = strings.TrimSpace
)