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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
// the local file; peerRef is the recipient. The file
// is read in-place from `path` on the sender's disk
// (no copy, no temp), so:
//   - the chat message on the sender's side uses the
//     original name (path's basename) and LocalPath =
//     path, so double-click / right-click in the
//     sender's chat opens the user's actual source file.
//   - the receiver gets the original name as the offer
//     name; if a file with that name already exists in
//     <data-dir>/received/, the receiver's rename
//     logic appends a counter ("name (1).ext") on its
//     own.
func (a *App) SendFile(peerRef, path string) string {
	if a.nd == nil {
		return "node not started"
	}
	if err := a.nd.SendFile(peerRef, path, "", path); err != nil {
		return err.Error()
	}
	return ""
}

// SendFileContent writes content to a temp file in
// <data-dir>/outbox/, then calls SendFile on it. The temp
// file is removed after the transfer completes (success
// or error). Used by the GUI's <input type="file"> picker
// which can't supply a real path (WebView2 has no
// File.path accessor; Wails v2.12 has no OpenFileDialog
// runtime API). The frontend reads the File via FileReader
// and passes the bytes here.
//
// On the sender's side, the chat message:
//   - has the user's original name (the File object's
//     name, no timestamp prefix, no rename),
//   - has LocalPath="" because the File API hides the
//     real on-disk path; the user knows where they
//     picked the file from and can find it themselves.
//
// We do NOT keep a permanent copy of picker-sourced
// files. The earlier "<data-dir>/sent/" copy was wrong:
// it re-renamed the file on collision and made the
// "open in chat" feature broken (the path the user
// clicked on was a copy in sent/, not the user's
// actual file).
func (a *App) SendFileContent(peerRef, name string, content []byte) string {
	if a.nd == nil {
		return "node not started"
	}
	if len(content) == 0 {
		return "empty file content"
	}
	if name == "" {
		return "empty file name"
	}
	// Sanitize: strip any directory components. The
	// user-facing name comes from a browser File object
	// and is already a basename, but we belt-and-suspenders
	// here so a future caller can't escape the data dir.
	cleanName := filepath.Base(name)
	if cleanName == "" || cleanName == "." || cleanName == "/" {
		return "invalid file name: " + name
	}
	// Write to <data-dir>/outbox/<name> as a temp file
	// and clean it up after the transfer. We use the
	// user's exact name (no timestamp, no "gui-" prefix,
	// no uniquePath suffix) so the on-the-wire offer
	// name matches what the user picked.
	outbox := filepath.Join(a.dataDir(), "outbox")
	if err := os.MkdirAll(outbox, 0o755); err != nil {
		return fmt.Sprintf("mkdir outbox: %v", err)
	}
	tmpPath := filepath.Join(outbox, cleanName)
	if err := os.WriteFile(tmpPath, content, 0o644); err != nil {
		return fmt.Sprintf("write outbox: %v", err)
	}
	// Best-effort cleanup; SendFile may copy + stream so
	// we give it a moment, then delete. The temp file is
	// short-lived by design — we don't want it sticking
	// around in outbox/ after the transfer.
	defer func() {
		time.Sleep(500 * time.Millisecond)
		_ = os.Remove(tmpPath)
	}()
	// Pass the user's original name (cleanName) as the
	// offer name. LocalPath="" because we don't have a
	// real source path on the sender's disk.
	if err := a.nd.SendFile(peerRef, tmpPath, cleanName, ""); err != nil {
		return err.Error()
	}
	return ""
}

// OpenPath opens a local file or folder with the OS
// default application. Used by the GUI to wire
// "double-click a file message to open" and
// "right-click → 打开所在文件夹". Returns "" on success
// or an error message on failure (e.g. file not found,
// or no association registered for that file type).
//
// On success, returns "". On Windows when the file has
// no default app association, we fall back to revealing
// the file in Explorer (which gives the user a chance to
// double-click it, and on first launch Windows shows
// the "Open with..." picker for the file type).
//
// Platform notes:
//   - Windows: `rundll32 url.dll,FileProtocolHandler <path>`
//     uses the registered file handler without spawning
//     a visible cmd window (unlike `cmd /c start`).
//     We HideWindow anyway for the no-association case
//     where the underlying call may briefly create one.
//   - macOS: `open <path>`.
//   - Linux: `xdg-open <path>`.
func (a *App) OpenPath(path string) string {
	if path == "" {
		return "path is empty"
	}
	if _, err := os.Stat(path); err != nil {
		return "file not found: " + path
	}
	err := openWithOS(path)
	if err == nil {
		return ""
	}
	// Fallback for Windows: when there's no default app
	// for the file type, the OS shows a "no association"
	// error popup. Reveal the file in Explorer instead —
	// the user can double-click it (which will show the
	// "Open with..." picker the first time) or pick
	// another file.
	if runtime.GOOS == "windows" {
		revealErr := revealInExplorer(path)
		if revealErr == nil {
			return ""
		}
		return "open failed: " + err.Error()
	}
	return err.Error()
}

// openWithOS is the per-OS "open with default app"
// launcher. Returns an error on failure (e.g. no
// association registered for the file type).
func openWithOS(path string) error {
	switch runtime.GOOS {
	case "windows":
		// rundll32 url.dll,FileProtocolHandler <path> — the
		// "shell open" path the browser uses; opens with
		// the registered handler and stays out of the way.
		// HideWindow avoids a brief console popup.
		cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", path)
		setSysProcAttr(cmd)
		if err := cmd.Start(); err != nil {
			return err
		}
		go func() { _ = cmd.Wait() }()
		return nil
	case "darwin":
		cmd := exec.Command("open", path)
		if err := cmd.Start(); err != nil {
			return err
		}
		go func() { _ = cmd.Wait() }()
		return nil
	default:
		cmd := exec.Command("xdg-open", path)
		if err := cmd.Start(); err != nil {
			return err
		}
		go func() { _ = cmd.Wait() }()
		return nil
	}
}

// RevealInFolder opens the OS file manager with the
// given file selected. Used by the right-click menu
// "打开文件所在文件夹" item. If path is a directory it
// just opens the directory without a selection.
//
// Platform notes:
//   - Windows: `explorer /select,<path>`.
//   - macOS: `open -R <path>` reveals in Finder.
//   - Linux: no standard reveal; falls back to opening
//     the parent directory via xdg-open.
func (a *App) RevealInFolder(path string) string {
	if path == "" {
		return "path is empty"
	}
	info, err := os.Stat(path)
	if err != nil {
		return "file not found: " + path
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		if err := revealInExplorer(path); err != nil {
			return "reveal failed: " + err.Error()
		}
		return ""
	case "darwin":
		cmd = exec.Command("open", "-R", path)
	default:
		target := path
		if !info.IsDir() {
			target = filepath.Dir(path)
		}
		cmd = exec.Command("xdg-open", target)
	}
	if err := cmd.Start(); err != nil {
		return "reveal failed: " + err.Error()
	}
	go func() { _ = cmd.Wait() }()
	return ""
}

// revealInExplorer is the per-OS "reveal" helper.
// Split out from RevealInFolder so OpenPath's
// fallback can reuse it on Windows.
func revealInExplorer(path string) error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("revealInExplorer: not Windows")
	}
	// Two-arg form: Go's runtime properly quotes the
	// second arg, so paths with spaces / parentheses
	// ("download (1).png") survive intact. The
	// single-arg "/select,<path>" form lost the spaces
	// in some Go runtime / Windows combos and the
	// Explorer window opened but the file selection
	// silently failed.
	cmd := exec.Command("explorer.exe", "/select,", path)
	setSysProcAttr(cmd)
	return cmd.Start()
}

// ReceivedFilePath returns the absolute path to a
// previously-received file by its base name, or "" if
// no such file is in <data-dir>/received/. The frontend
// uses this to make file messages in the chat history
// (re-loaded from chat.enc on startup, where the live
// LocalPath was lost) clickable again.
func (a *App) ReceivedFilePath(name string) string {
	if name == "" {
		return ""
	}
	dir := filepath.Join(a.dataDir(), "received")
	// Try the name as-is first, then URL-decode (some
	// browsers / file-association flows percent-encode
	// non-ASCII names; we try both).
	candidates := []string{name}
	if decoded, err := url.PathUnescape(name); err == nil && decoded != name {
		candidates = append(candidates, decoded)
	}
	for _, c := range candidates {
		p := filepath.Join(dir, c)
		if info, err := os.Stat(p); err == nil && !info.IsDir() {
			return p
		}
	}
	return ""
}

// uniquePath returns a path in dir for a file named
// `name` that doesn't yet exist. If `name` itself is
// free, it's returned as-is. Otherwise, "name (1)",
// "name (2)", ... are tried. The result is in the
// format "stem (N).ext" so the suffix is recognisable
// in Explorer / Finder / Nautilus.
//
// We don't ask the user "overwrite / rename / cancel"
// for v0.1 — the product copy says direct-save with
// collision-safe names.
func uniquePath(dir, name string) string {
	candidate := filepath.Join(dir, name)
	if _, err := os.Stat(candidate); err != nil {
		return candidate
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; i < 10000; i++ {
		c := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
		if _, err := os.Stat(c); err != nil {
			return c
		}
	}
	// pathological fallback: 10000 collisions on the same
	// stem. Append a nanosecond stamp to keep the file
	// from being silently overwritten.
	return filepath.Join(dir, fmt.Sprintf("%s-%d%s", stem, time.Now().UnixNano(), ext))
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