package main

// Group REPL commands (v1.1, 2026-06-27).
//
// Each handler is a thin wrapper around the corresponding
// pkg/node public method. The wrappers exist only to format
// CLI-friendly error and status messages — every real action
// lives in pkg/node.
//
// Sub-commands:
//
//	group list                       — list all groups this peer knows about
//	group create <name> [peer ...]   — create a group with optional invitees
//	group show <group-id>            — show one group's roster
//	group invite <group-id> <peer>   — sign + send an invite (1:1)
//	group send <group-id> <text>     — broadcast a text to every member
//	group history <group-id>         — show recent chat in one group
//	group leave <group-id>           — drop a group (creator can't leave)
//
// GroupIDs are content-addressed (SM3 hash). The CLI accepts
// the rendered "g_<64hex>" form for ergonomics.

import (
	"log"
	"strings"

	"github.com/weishengsuptp/innerlink/pkg/group"
	"github.com/weishengsuptp/innerlink/pkg/node"
)

func cmdGroup(nd *node.Node, parts []string) {
	if len(parts) < 2 {
		cmdGroupHelp()
		return
	}
	sub := parts[1]
	rest := parts[2:]
	switch sub {
	case "list", "ls":
		cmdGroupList(nd)
	case "create", "new":
		cmdGroupCreate(nd, rest)
	case "show", "info":
		cmdGroupShow(nd, rest)
	case "invite", "add":
		cmdGroupInvite(nd, rest)
	case "send", "msg":
		cmdGroupSend(nd, rest)
	case "history":
		cmdGroupHistory(nd, rest)
	case "leave", "exit", "quit":
		cmdGroupLeave(nd, rest)
	case "help":
		cmdGroupHelp()
	default:
		log.Printf("[USAGE] group: unknown sub-command %q (type 'group help')", sub)
	}
}

func cmdGroupHelp() {
	log.Println("[HELP ] group list                            -- list all groups I belong to")
	log.Println("[HELP ] group create <name> [peer ...]        -- create a group with the named members")
	log.Println("[HELP ] group show <group-id>                 -- show one group's roster + members")
	log.Println("[HELP ] group invite <group-id> <peer>        -- send a 1:1 invite to <peer>")
	log.Println("[HELP ] group send <group-id> <text>          -- broadcast a message to every member")
	log.Println("[HELP ] group history <group-id>              -- show recent chat in one group")
	log.Println("[HELP ] group leave <group-id>                -- leave a group (creator cannot leave)")
	log.Println("[HELP ] group help                            -- this list")
}

func cmdGroupList(nd *node.Node) {
	groups, err := nd.ListGroups()
	if err != nil {
		log.Printf("[ERROR] group list: %v", err)
		return
	}
	if len(groups) == 0 {
		log.Printf("[INFO ] no groups yet (create one with 'group create')")
		return
	}
	log.Printf("[GROUP ] %d group(s):", len(groups))
	for _, g := range groups {
		marker := " "
		if g.Self {
			marker = "*" // I'm a member
		}
		log.Printf("[GROUP ] %s %s  %q  (%d member(s))  creator=%s",
			marker, g.GroupID, g.GroupName, len(g.Members), shortHex(g.Creator))
	}
}

func cmdGroupCreate(nd *node.Node, rest []string) {
	if len(rest) < 1 {
		log.Println("[USAGE] group create <name> [peer-id-or-alias ...]")
		return
	}
	name := rest[0]
	members := rest[1:]
	// Resolve aliases to peerIDs (CreateGroup accepts either).
	resolved := make([]string, 0, len(members))
	for _, m := range members {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		// resolvePeerRef is unexported in main.go; we look
		// it up via the alias store instead. If the input
		// is already a 32-char hex PeerID, the alias
		// lookup returns "" and we use the input as-is.
		resolved = append(resolved, m)
	}
	info, err := nd.CreateGroup(name, resolved)
	if err != nil {
		log.Printf("[ERROR] group create: %v", err)
		return
	}
	log.Printf("[GROUP ] created %s  name=%q  members=%d", info.GroupID, info.GroupName, len(info.Members))
	log.Printf("[GROUP ] invite members with: group invite %s <peer-id-or-alias>", info.GroupID)
}

func cmdGroupShow(nd *node.Node, rest []string) {
	if len(rest) < 1 {
		log.Println("[USAGE] group show <group-id>")
		return
	}
	rawID, err := group.ParseGroupID(rest[0])
	if err != nil {
		log.Printf("[ERROR] group show: bad GroupID: %v", err)
		return
	}
	info, err := nd.GetGroup(group.RenderGroupID(rawID))
	if err != nil {
		log.Printf("[ERROR] group show %s: %v", rest[0], err)
		return
	}
	log.Printf("[GROUP ] %s  name=%q  creator=%s  members=%d  self_member=%v",
		info.GroupID, info.GroupName, shortHex(info.Creator), len(info.Members), info.Self)
	for _, m := range info.Members {
		marker := " "
		if m == info.Creator {
			marker = "C" // creator
		} else if m == ndSelfID(nd) {
			marker = "*" // me
		}
		log.Printf("[GROUP ]   %s %s", marker, shortHex(m))
	}
}

func cmdGroupInvite(nd *node.Node, rest []string) {
	if len(rest) < 2 {
		log.Println("[USAGE] group invite <group-id> <peer-id-or-alias>")
		return
	}
	rawID, err := group.ParseGroupID(rest[0])
	if err != nil {
		log.Printf("[ERROR] group invite: bad GroupID: %v", err)
		return
	}
	inv, err := nd.InviteToGroup(rawID, rest[1])
	if err != nil {
		log.Printf("[ERROR] group invite: %v", err)
		return
	}
	log.Printf("[GROUP ] invited %s to %s (nonce=%x...)",
		rest[1], group.RenderGroupID(rawID), inv.Nonce[:4])
	log.Printf("[GROUP ] invite expires in 24h; recipient auto-accepts on receive (v1.1)")
}

func cmdGroupSend(nd *node.Node, rest []string) {
	if len(rest) < 2 {
		log.Println("[USAGE] group send <group-id> <text>")
		return
	}
	rawID, err := group.ParseGroupID(rest[0])
	if err != nil {
		log.Printf("[ERROR] group send: bad GroupID: %v", err)
		return
	}
	text := strings.Join(rest[1:], " ")
	if err := nd.SendGroupMessage(rawID, text); err != nil {
		log.Printf("[ERROR] group send: %v", err)
		return
	}
}

func cmdGroupHistory(nd *node.Node, rest []string) {
	if len(rest) < 1 {
		log.Println("[USAGE] group history <group-id>")
		return
	}
	rawID, err := group.ParseGroupID(rest[0])
	if err != nil {
		log.Printf("[ERROR] group history: bad GroupID: %v", err)
		return
	}
	msgs, err := nd.HistoryGroup(group.RenderGroupID(rawID))
	if err != nil {
		log.Printf("[ERROR] group history: %v", err)
		return
	}
	if len(msgs) == 0 {
		log.Printf("[INFO ] no chat history for this group yet")
		return
	}
	start := 0
	if len(msgs) > 50 {
		start = len(msgs) - 50
	}
	log.Printf("[GROUP ] showing last %d of %d message(s):", len(msgs)-start, len(msgs))
	for _, m := range msgs[start:] {
		dirMark := "<"
		if m.Direction == "out" {
			dirMark = ">"
		}
		log.Printf("[GROUP ] %s %s  %s", m.Timestamp.Format("15:04:05"), dirMark, m.Body)
	}
}

func cmdGroupLeave(nd *node.Node, rest []string) {
	if len(rest) < 1 {
		log.Println("[USAGE] group leave <group-id>")
		return
	}
	rawID, err := group.ParseGroupID(rest[0])
	if err != nil {
		log.Printf("[ERROR] group leave: bad GroupID: %v", err)
		return
	}
	if err := nd.LeaveGroup(group.RenderGroupID(rawID)); err != nil {
		log.Printf("[ERROR] group leave: %v", err)
		return
	}
	log.Printf("[GROUP ] left %s (local cleanup done)", rest[0])
}

// shortHex returns the first 12 chars of a hex peerID for
// friendly CLI logs (avoids 32-char walls in the terminal).
func shortHex(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

// ndSelfID returns this node's PeerID (hex). Wrapper so
// the help/list output reads correctly; used in group
// show to mark "me" with `*`.
func ndSelfID(nd *node.Node) string {
	return nd.SelfPeerID()
}
