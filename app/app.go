// Package app contains the innerlink Wails app: the
// bridge between the Go protocol layer (pkg/node) and
// the TypeScript frontend.
//
// Lifecycle:
//
//   - Wails calls NewApp before main() and binds the
//     returned App to the JS bridge.
//   - OnStartup fires once the WebView2 / WKWebView /
//     WebKitGTK frontend is up; we construct and Start
//     the Node, then begin forwarding peer / message
//     events to the frontend via Wails runtime events.
//   - OnShutdown fires after the user closes the window
//     and Wails is tearing down; we Close() the Node
//     synchronously so its goroutines actually finish
//     before main() returns. This is the only
//     "shutdown" hook we need; nothing else.
//
// This file is deliberately small. The protocol layer
// in pkg/node is the product; this is glue.
//
// History note (2026-06): earlier innerlink-desktop
// prototypes accumulated close-exit workarounds
// (OnBeforeClose, killWebView2Children, Windows Job
// Object, SetWinEventHook, runtime.Quit fallbacks).
// Those are all gone in this rewrite. The original
// close bug was the Node's pump goroutines blocking
// on `for ev := range ch` while Wails quit; calling
// nd.Close() before main returns closes those channels,
// the pumps fall out, the runtime is free to exit.
// This app.go is the textbook Wails shape; if the
// close bug returns, the bug is upstream, not here.
package app

import (
	"context"
	"log"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/weishengsuptp/innerlink/pkg/node"
)

// App is bound to the Wails frontend. Every exported
// method becomes a callable from JS; every field is
// state visible to the bridge.
//
// Only fields the frontend needs should be exported.
type App struct {
	ctx context.Context // Wails runtime context, set in OnStartup
	nd  *node.Node      // nil until OnStartup, never reset
}

// NewApp returns an App ready to be bound to the
// Wails JS bridge. Use this from main.go:
//
//	a := app.NewApp()
//	wails.Run(&options.App{Bind: []interface{}{a}, OnStartup: a.startup, OnShutdown: a.shutdown})
func NewApp() *App {
	return &App{}
}

// Startup is the Wails OnStartup hook. Wails calls it
// once the frontend is up. We:
//
//  1. Stash the Wails context (so bound methods can
//     log + emit events).
//  2. Construct + Start the protocol Node with default
//     options (data dir ./.innerlink, UDP/TCP default
//     ports, info-level logging to ./innerlink.log).
//  3. Start two background pump goroutines that
//     forward SubscribePeers / SubscribeMessages events
//     to the frontend as Wails runtime events.
//
// Pump goroutines exit when their source channel is
// closed, which happens when Node.Close() is called in
// OnShutdown. No explicit WaitGroup needed; the Node's
// own WG already tracks these pumps (via its dispatcher
// fan-in).
func (a *App) Startup(ctx context.Context) {
	a.ctx = ctx

	nd, err := node.New(node.Options{})
	if err != nil {
		log.Fatalf("innerlink: start: %v", err)
	}
	if err := nd.Start(ctx); err != nil {
		log.Fatalf("innerlink: node.Start: %v", err)
	}
	a.nd = nd

	go a.pumpPeers()
	go a.pumpMessages()
}

// Shutdown is the Wails OnShutdown hook. Wails calls it
// after the user closes the window. We Close() the Node
// synchronously, which:
//   - closes the SubscribePeers / SubscribeMessages channels
//   - tears down UDP / TCP listeners
//   - waits on its internal WaitGroup for all goroutines
//
// After Close returns, there are no live goroutines owned
// by us; main() may return safely.
func (a *App) Shutdown(_ context.Context) {
	if a.nd != nil {
		if err := a.nd.Close(); err != nil {
			log.Printf("[WARN] node close: %v", err)
		}
		a.nd = nil
	}
}

// ---- bound methods (callable from JS) ----
//
// Convention: error returns are exposed as plain strings
// so the JS side can check `if (result !== "") throw`.
// Nil means success. This is more ergonomic than Wails'
// default (which surfaces Go errors as JS exceptions with
// only the message string anyway).

// SelfPeerID returns this device's 32-char hex peer ID.
func (a *App) SelfPeerID() string {
	if a.nd == nil {
		return ""
	}
	return a.nd.SelfPeerID()
}

// ListPeers returns a snapshot of known peers. JS calls
// this on app start, after reconnecting to the LAN, etc.
func (a *App) ListPeers() []node.PeerInfo {
	if a.nd == nil {
		return nil
	}
	return a.nd.ListPeers()
}

// ListAliases returns the alias table (name → peer ID).
func (a *App) ListAliases() []node.Alias {
	if a.nd == nil {
		return nil
	}
	return a.nd.ListAliases()
}

// SendText sends a chat message. peerRef is an alias
// name or a 32-char hex PeerID. Returns "" on success,
// error message otherwise.
func (a *App) SendText(peerRef, text string) string {
	if a.nd == nil {
		return "node not started"
	}
	if err := a.nd.SendText(peerRef, text); err != nil {
		return err.Error()
	}
	return ""
}

// SendFile starts an out-of-band file transfer. path is
// the local file; peerRef is the recipient.
func (a *App) SendFile(peerRef, path string) string {
	if a.nd == nil {
		return "node not started"
	}
	if err := a.nd.SendFile(peerRef, path); err != nil {
		return err.Error()
	}
	return ""
}

// Ping sends a one-shot probe to a peer; useful to
// force the handshake / roster exchange without
// sending a real chat.
func (a *App) Ping(peerRef string) string {
	if a.nd == nil {
		return "node not started"
	}
	if err := a.nd.Ping(peerRef); err != nil {
		return err.Error()
	}
	return ""
}

// DialAddr connects to a specific "ip:port" without
// relying on UDP discovery (e.g. across subnets).
func (a *App) DialAddr(addr string) string {
	if a.nd == nil {
		return "node not started"
	}
	if err := a.nd.DialAddr(addr); err != nil {
		return err.Error()
	}
	return ""
}

// SetAlias assigns a friendly name to a peer.
func (a *App) SetAlias(peerRef, name string) string {
	if a.nd == nil {
		return "node not started"
	}
	if err := a.nd.SetAlias(peerRef, name); err != nil {
		return err.Error()
	}
	return ""
}

// RemoveAlias clears the alias mapping for ref (peer
// ID or alias name).
func (a *App) RemoveAlias(ref string) string {
	if a.nd == nil {
		return "node not started"
	}
	if err := a.nd.RemoveAlias(ref); err != nil {
		return err.Error()
	}
	return ""
}

// History returns the in-memory chat history for one
// peer. JS calls this when opening a chat panel.
func (a *App) History(peerRef string) []node.Message {
	if a.nd == nil {
		return nil
	}
	return a.nd.History(peerRef)
}

// Scan triggers a one-shot subnet scan. cidr is e.g.
// "192.168.40.0/24". Returns "" on success.
func (a *App) Scan(cidr string) string {
	if a.nd == nil {
		return "node not started"
	}
	if err := a.nd.Scan(a.ctx, cidr); err != nil {
		return err.Error()
	}
	return ""
}

// ---- pump goroutines ----

// pumpPeers forwards peer transitions to the frontend
// as "peer:event" runtime events. Exits when
// SubscribePeers' channel is closed (Node.Close).
func (a *App) pumpPeers() {
	for ev := range a.nd.SubscribePeers() {
		wruntime.EventsEmit(a.ctx, "peer:event", ev)
	}
}

// pumpMessages forwards chat messages to the frontend
// as "message" runtime events. Exits when
// SubscribeMessages' channel is closed.
func (a *App) pumpMessages() {
	for msg := range a.nd.SubscribeMessages() {
		wruntime.EventsEmit(a.ctx, "message", msg)
	}
}

