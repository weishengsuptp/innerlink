// Group event plumbing — mirror of peers.go PeerEvent
// so the GUI can subscribe to group transitions via
// Node.SubscribeGroups(). The app layer forwards each
// event as a "group:event" runtime event, which the
// frontend listens on to refresh the sidebar's "群组"
// section and surface an "你被拉进群 X" toast on
// invite-received. v1.1 (2026-06-28).

package node

// GroupEventType enumerates the kinds of transitions
// delivered on SubscribeGroups. Two for now; expand
// when SenderKeys distribution adds membership /
// re-key / dissolve events.
type GroupEventType string

const (
	// GroupAdded fires when a new group appears on our
	// local disk. Two paths produce it:
	//   - we just called CreateGroup (we're the creator)
	//   - we just auto-accepted a TypeGroupInvite
	//     envelope (InviterHex identifies who pulled us in;
	//     empty for self-created groups)
	// The frontend reloads ListGroups on every GroupAdded
	// so the sidebar is always in sync without having to
	// embed the full GroupInfo in the event payload.
	GroupAdded GroupEventType = "added"
	// GroupRemoved fires when our local copy of a group
	// is deleted. Currently only LeaveGroup produces this
	// (creator-dissolve is not yet a GUI option).
	GroupRemoved GroupEventType = "removed"
)

// GroupEvent is one transition. SubscribeGroups delivers
// these as they happen.
type GroupEvent struct {
	Type GroupEventType
	// GroupID is the rendered "g_<64hex>" form so the
	// frontend can use it directly as the conversation
	// key (no rendering round-trip needed).
	GroupID string
	// GroupName is the human-readable name. Carried in
	// the event payload so the "你被拉进群 X" toast
	// doesn't need a second Wails round-trip to fetch
	// it. Best-effort — empty for events fired before
	// the on-disk members.json was readable.
	GroupName string
	// InviterHex is set on GroupAdded when the path is
	// "someone invited us"; empty when the path is
	// "we created it ourselves". The frontend uses this
	// to render "Alice 把你拉进了「饭局」群" instead of
	// a generic "新群组" toast.
	InviterHex string
}

// SubscribeGroups returns a channel of group transitions.
// Buffered to 32; drops oldest under sustained flood.
// Group creation / invite are user-initiated actions
// (rate-limited by hand), so 32 is plenty of headroom.
// The channel is closed when Close() is called.
func (n *Node) SubscribeGroups() <-chan GroupEvent {
	return n.groupEventCh
}

// publishGroupEvent drops an event into the SubscribeGroups
// channel with drop-oldest backpressure (mirrors
// publishPeerEvent's pattern in peers.go).
func (n *Node) publishGroupEvent(ev GroupEvent) {
	if n.groupEventCh == nil {
		return
	}
	select {
	case n.groupEventCh <- ev:
	default:
		// Channel full: drop oldest, then re-try once.
		// If still full (a goroutine is parked on send),
		// the event is lost — the GUI can re-poll
		// ListGroups() if it detects staleness.
		select {
		case <-n.groupEventCh:
		default:
		}
		select {
		case n.groupEventCh <- ev:
		default:
		}
	}
}
