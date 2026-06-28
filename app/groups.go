package app

// Group Wails bindings (v1.1, 2026-06-27).
//
// Each method is a thin wrapper around the corresponding
// pkg/node public method, mirroring the existing pattern
// for History / ClearHistory / SendFile / etc. The wrappers
// exist only to:
//   - Return "" / structured error instead of bare error
//     so the TS bridge gets a uniform "Err" string field
//   - Defensive nil checks on a.nd (the Node may not be
//     up yet if the GUI hits a binding before OnStartup
//     finishes)
//   - Hide internal types (e.g. the raw 32-byte GroupID)
//     from the JS layer where possible — GroupInfo /
//     Message types are TS-friendly
//
// Naming convention (v1.1):
//   - Get*   — read-only lookups (ListGroups, GetGroup)
//   - Create/Invite/Send/Leave — mutating operations
//   - All return either a struct (success) or "" + Err
//     field for explicit error reporting
//
// Group IDs flow as the rendered "g_<64hex>" string the
// GUI shows in the sidebar. JS code never touches raw
// 32-byte slices.

import (
	"strings"

	"github.com/weishengsuptp/innerlink/pkg/group"
	"github.com/weishengsuptp/innerlink/pkg/node"
)

// CreateGroupResult mirrors pkg/node.GroupInfo for the
// Wails TS bridge (Wails generates a TS interface from
// the struct fields). We alias rather than re-export so
// the GUI has a stable type name.
type CreateGroupResult struct {
	GroupInfo node.GroupInfo `json:"group_info"`
	Err       string         `json:"err"`
}

// ListGroupsResult wraps the slice so the GUI always
// gets a non-nil array (TypeScript null-checks are noisy).
type ListGroupsResult struct {
	Groups []node.GroupInfo `json:"groups"`
	Err    string           `json:"err"`
}

// GroupMessageResult is the success-or-error envelope
// for invite / send / leave operations. Empty Err +
// non-empty Status = success; non-empty Err = failure.
type GroupMessageResult struct {
	Status string `json:"status"`
	Err    string `json:"err"`
}

// GroupHistoryResult wraps HistoryGroup output.
type GroupHistoryResult struct {
	Messages []node.Message `json:"messages"`
	Err      string         `json:"err"`
}

// CreateGroup creates a new group and returns its
// GroupInfo. members is a slice of peer IDs / aliases
// (matches pkg/node.CreateGroup semantics).
func (a *App) CreateGroup(name string, members []string) CreateGroupResult {
	if a.nd == nil {
		return CreateGroupResult{Err: "node not started"}
	}
	info, err := a.nd.CreateGroup(strings.TrimSpace(name), members)
	if err != nil {
		return CreateGroupResult{Err: err.Error()}
	}
	return CreateGroupResult{GroupInfo: *info}
}

// ListGroups returns every group on disk. Self=true
// means our peerID is in the roster.
func (a *App) ListGroups() ListGroupsResult {
	if a.nd == nil {
		return ListGroupsResult{Err: "node not started"}
	}
	gs, err := a.nd.ListGroups()
	if err != nil {
		return ListGroupsResult{Err: err.Error()}
	}
	if gs == nil {
		gs = []*node.GroupInfo{}
	}
	// Convert []*GroupInfo → []node.GroupInfo for TS
	// (Wails serializes slices cleanly).
	out := make([]node.GroupInfo, 0, len(gs))
	for _, g := range gs {
		out = append(out, *g)
	}
	return ListGroupsResult{Groups: out}
}

// GetGroup fetches one group's roster.
func (a *App) GetGroup(renderedID string) CreateGroupResult {
	if a.nd == nil {
		return CreateGroupResult{Err: "node not started"}
	}
	rawID, err := group.ParseGroupID(renderedID)
	if err != nil {
		return CreateGroupResult{Err: "bad GroupID: " + err.Error()}
	}
	info, err := a.nd.GetGroup(group.RenderGroupID(rawID))
	if err != nil {
		return CreateGroupResult{Err: err.Error()}
	}
	return CreateGroupResult{GroupInfo: *info}
}

// InviteToGroup signs + sends a 1:1 invite envelope.
// inviteePeerID may be an alias or a 32-char hex peerID.
func (a *App) InviteToGroup(renderedID, inviteePeerID string) GroupMessageResult {
	if a.nd == nil {
		return GroupMessageResult{Err: "node not started"}
	}
	rawID, err := group.ParseGroupID(renderedID)
	if err != nil {
		return GroupMessageResult{Err: "bad GroupID: " + err.Error()}
	}
	if _, err := a.nd.InviteToGroup(rawID, inviteePeerID); err != nil {
		return GroupMessageResult{Err: err.Error()}
	}
	return GroupMessageResult{Status: "invited " + inviteePeerID}
}

// SendGroupMessage broadcasts text to every online
// member. v1.1: plaintext over per-member channels
// (SenderKeys distribution lands in a follow-up).
func (a *App) SendGroupMessage(renderedID, text string) GroupMessageResult {
	if a.nd == nil {
		return GroupMessageResult{Err: "node not started"}
	}
	rawID, err := group.ParseGroupID(renderedID)
	if err != nil {
		return GroupMessageResult{Err: "bad GroupID: " + err.Error()}
	}
	if err := a.nd.SendGroupMessage(rawID, text); err != nil {
		return GroupMessageResult{Err: err.Error()}
	}
	return GroupMessageResult{Status: "sent"}
}

// HistoryGroup returns the chat records for one group.
func (a *App) HistoryGroup(renderedID string) GroupHistoryResult {
	if a.nd == nil {
		return GroupHistoryResult{Err: "node not started"}
	}
	rawID, err := group.ParseGroupID(renderedID)
	if err != nil {
		return GroupHistoryResult{Err: "bad GroupID: " + err.Error()}
	}
	msgs, err := a.nd.HistoryGroup(group.RenderGroupID(rawID))
	if err != nil {
		return GroupHistoryResult{Err: err.Error()}
	}
	if msgs == nil {
		msgs = []node.Message{}
	}
	return GroupHistoryResult{Messages: msgs}
}

// LeaveGroup drops a group's local directory.
// Returns an error if the caller is the creator (creator
// must use the future DissolveGroup, not yet implemented).
func (a *App) LeaveGroup(renderedID string) GroupMessageResult {
	if a.nd == nil {
		return GroupMessageResult{Err: "node not started"}
	}
	rawID, err := group.ParseGroupID(renderedID)
	if err != nil {
		return GroupMessageResult{Err: "bad GroupID: " + err.Error()}
	}
	if err := a.nd.LeaveGroup(group.RenderGroupID(rawID)); err != nil {
		return GroupMessageResult{Err: err.Error()}
	}
	return GroupMessageResult{Status: "left"}
}

// SendGroupFileResult mirrors SendFilePathResult for the
// group case. fileID is the BASE id (one per logical send);
// the backend derives per-member ids by appending "_<hex>".
// fileIDs lists the per-member ids that actually went out
// (online members only) — the GUI listens on file:event
// for each to drive the live progress bubble.
type SendGroupFileResult struct {
	FileID  string   `json:"fileID"`  // base fileID (GUI uses this as the conversation key)
	Sent    int      `json:"sent"`    // count of online members the file was sent to
	FileIDs []string `json:"fileIDs"` // per-member fileIDs that were dispatched
	Err     string   `json:"err"`
}

// SendGroupFile broadcasts filePath to every online member
// of the group. Returns the base fileID so the GUI can
// wire up a single live progress bubble per send (per-
// member file:event streams are derived from this base).
//
// Caller (frontend) supplies baseFileID so the placeholder
// bubble can be inserted into the chat panel BEFORE the
// Go side responds — matches the 1:1 SendFile pattern.
//
// v1.1 (2026-06-28).
func (a *App) SendGroupFile(renderedID, filePath, baseFileID string) SendGroupFileResult {
	if a.nd == nil {
		return SendGroupFileResult{Err: "node not started"}
	}
	rawID, err := group.ParseGroupID(renderedID)
	if err != nil {
		return SendGroupFileResult{Err: "bad GroupID: " + err.Error()}
	}
	delivered, err := a.nd.SendGroupFile(rawID, filePath, baseFileID)
	if err != nil {
		return SendGroupFileResult{Err: err.Error(), FileID: baseFileID}
	}
	return SendGroupFileResult{
		FileID:  baseFileID,
		FileIDs: delivered,
		Sent:    len(delivered),
	}
}
