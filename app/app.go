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
// Close-exit debugging (2026-06-23):
// The previous rewrite claimed "default Wails quit chain
// works as long as Shutdown tears down the core
// goroutines before main returns". Reality: WM_CLOSE
// on Win10 1909 leaves the process alive ~forever. We
// don't write a workaround yet; we instrument every
// step to see exactly where the chain hangs. Once we
// know, we decide whether to fix core, fix Wails
// config, or accept the upstream bug and document it.
//
// The protocol layer in pkg/node is the product; this
// is glue.
package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"time"

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
	log.Printf("[INFO  app] Startup ENTER (goroutines=%d)", runtime.NumGoroutine())
	a.ctx = ctx

	nd, err := node.New(node.Options{})
	if err != nil {
		log.Printf("[ERROR app] node.New failed: %v", err)
		log.Fatalf("innerlink: start: %v", err)
	}
	log.Printf("[INFO  app] node.New OK (goroutines=%d)", runtime.NumGoroutine())

	if err := nd.Start(ctx); err != nil {
		log.Printf("[ERROR app] node.Start failed: %v", err)
		log.Fatalf("innerlink: node.Start: %v", err)
	}
	log.Printf("[INFO  app] node.Start OK (goroutines=%d)", runtime.NumGoroutine())

	a.nd = nd

	go a.pumpPeers()
	go a.pumpMessages()
	log.Printf("[INFO  app] Startup RETURN (goroutines=%d)", runtime.NumGoroutine())
}

// BeforeClose is the Wails OnBeforeClose hook. Wails
// calls it BEFORE actually destroying the window when
// the user clicks X. Returning false (not prevent) lets
// the close proceed; we use this hook ONLY for
// diagnostic logging so we can tell whether Wails is
// reaching its own quit chain at all on Win10 1909.
//
// We do NOT do real work here — moving nd.Close here
// would deadlock the Wails quit chain (previous
// prototypes proved this with v0.1.11/v0.1.12).
func (a *App) BeforeClose(_ context.Context) (prevent bool) {
	log.Printf("[INFO  app] BeforeClose ENTER (goroutines=%d, nd=%v)", runtime.NumGoroutine(), a.nd != nil)
	return false
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
//
// We measure the time it takes — if Close takes > 5s,
// something is hung (the previous rewrite was
// unobservable here because all logs were dropped when
// logx had no flush hook).
func (a *App) Shutdown(_ context.Context) {
	log.Printf("[INFO  app] Shutdown ENTER (goroutines=%d, nd=%v)", runtime.NumGoroutine(), a.nd != nil)
	if a.nd == nil {
		log.Printf("[INFO  app] Shutdown: nd is nil, nothing to close")
		return
	}
	t0 := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- a.nd.Close()
	}()
	var closeErr error
	select {
	case closeErr = <-done:
		log.Printf("[INFO  app] nd.Close returned in %v (goroutines=%d)", time.Since(t0), runtime.NumGoroutine())
	case <-time.After(5 * time.Second):
		log.Printf("[WARN app] nd.Close HUNG > 5s, dumping goroutines:")
		buf := make([]byte, 1<<16)
		n := runtime.Stack(buf, true)
		log.Printf("[WARN app] %s", buf[:n])
		closeErr = <-done // still wait, so we don't race with logx
	}
	if closeErr != nil {
		log.Printf("[WARN app] node close: %v", closeErr)
	}
	a.nd = nil
	log.Printf("[INFO  app] Shutdown RETURN (goroutines=%d)", runtime.NumGoroutine())
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

// SendFileContent writes content to a temp file in
// <data-dir>/outbox/, then calls SendFile on it. The temp
// file is removed after the transfer completes (success or
// error). Used by the GUI's <input type="file"> picker
// which can't supply a real path (WebView2 has no
// File.path accessor; Wails v2.12 has no OpenFileDialog
// runtime API). The frontend reads the File via FileReader
// and passes the bytes here.
func (a *App) SendFileContent(peerRef, name string, content []byte) string {
	if a.nd == nil {
		return "node not started"
	}
	if len(content) == 0 {
		return "empty file content"
	}
	// derive data dir from the persisted state. node.Options
	// defaults DataDir to <cwd>/.innerlink/ which is where
	// logx put us. We mirror that: walk the active layout
	// via the node's persisted store dir.
	outbox := filepath.Join(a.dataDir(), "outbox")
	if err := os.MkdirAll(outbox, 0o755); err != nil {
		return fmt.Sprintf("mkdir outbox: %v", err)
	}
	tmpName := fmt.Sprintf("gui-%d-%s", time.Now().UnixNano(), filepath.Base(name))
	tmpPath := filepath.Join(outbox, tmpName)
	if err := os.WriteFile(tmpPath, content, 0o644); err != nil {
		return fmt.Sprintf("write temp: %v", err)
	}
	// best-effort cleanup; SendFile may copy + stream so we
	// give it a moment, then delete.
	defer func() {
		time.Sleep(500 * time.Millisecond)
		_ = os.Remove(tmpPath)
	}()
	if err := a.nd.SendFile(peerRef, tmpPath); err != nil {
		return err.Error()
	}
	return ""
}

// dataDir returns the persistent state directory used by
// the node. Innerlink-core exposes this via internal/paths
// but we keep it simple: respect the cwd override the node
// was created with, or default to <launch-cwd>/.innerlink/.
// Wails v2 on Windows sets cwd to %APPDATA%\Roaming\<bin>\.
// We re-derive that here so SendFileContent's outbox/ lives
// in the same place as chat.enc, device.key, etc.
func (a *App) dataDir() string {
	// Heuristic: walk from cwd. If a .innerlink/ exists here
	// use it; otherwise default to ./.innerlink/.
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	candidate := filepath.Join(cwd, ".innerlink")
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return candidate
	}
	return filepath.Join(cwd, ".innerlink")
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
	log.Printf("[INFO  app] pumpPeers ENTER (goroutines=%d)", runtime.NumGoroutine())
	defer log.Printf("[INFO  app] pumpPeers EXIT (goroutines=%d)", runtime.NumGoroutine())
	for ev := range a.nd.SubscribePeers() {
		log.Printf("[INFO  app] pumpPeers emit peer:event %+v", ev)
		wruntime.EventsEmit(a.ctx, "peer:event", ev)
	}
}

// pumpMessages forwards chat messages to the frontend
// as "message:event" runtime events. Exits when
// SubscribeMessages' channel is closed.
func (a *App) pumpMessages() {
	log.Printf("[INFO  app] pumpMessages ENTER (goroutines=%d)", runtime.NumGoroutine())
	defer log.Printf("[INFO  app] pumpMessages EXIT (goroutines=%d)", runtime.NumGoroutine())
	for msg := range a.nd.SubscribeMessages() {
		log.Printf("[INFO  app] pumpMessages emit message:event %+v", msg)
		wruntime.EventsEmit(a.ctx, "message:event", msg)
	}
}