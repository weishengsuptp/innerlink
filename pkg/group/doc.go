// Package group implements the v1.1 group-chat primitives on top of
// the v2 protocol envelope (internal/protocol).
//
// Layering (this package sits between storage and node):
//
//	pkg/group/         <-- this package: pure data + crypto, no I/O
//	  internal/crypto  SM3 hash, SM2 sign/verify
//	  internal/protocol Envelope v2 carries GroupID
//	  storage layer    (next) reads/writes <data-dir>/groups/<id>/
//	pkg/node/          (next) wires group into the message bus
//	app/               (next) exposes App.CreateGroup etc. to Wails
//	frontend           (last)  sidebar + group chat panel
//
// Why a new package and not extending pkg/node:
//
//   - Group state has its own invariants (members.json is signed by
//     creator; sender keys are per-(group,sender); invites bind
//     (groupID, inviter, signature)). Mixing them into pkg/node
//     would bloat node.go past the ~1500-line readability cliff.
//   - Pure-data package is trivially unit-testable without spinning
//     up a Node or transport. Tests in this file exercise the
//     crypto and storage shapes directly.
//   - The Sender Keys distribution flow lives here, not in node —
//     it's a group-internal protocol that doesn't depend on the
//     wider node lifecycle.
//
// Out of scope for this package (handled elsewhere):
//
//   - The actual on-wire protocol for sending group messages
//     (uses internal/protocol.Envelope with GroupID set — see
//     protocol.ProtocolVersion = 2).
//   - Storage layout under <data-dir>/groups/ (this package
//     defines paths but the Read/Write methods on Members
//     depend on storage package additions).
//   - User-facing CLI (cmd/innerlink/repl.go).
package group
