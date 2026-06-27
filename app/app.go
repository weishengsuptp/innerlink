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
	"crypto/rand"
	"encoding/hex"
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
	// (the previous picker streaming route used a
	// pendingFiles map for chunk staging; the picker now
	// uses runtime.OpenFileDialog + SendFilePath, no
	// map needed)
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
	go a.pumpFiles()
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
//
// Drag-and-drop + CLI route: the caller already has a
// path. core opens the file itself and runs the
// transfer; the frontend never sees the bytes.
func (a *App) SendFile(peerRef, path string) SendFilePathResult {
	if a.nd == nil {
		return SendFilePathResult{Err: "node not started"}
	}
	if peerRef == "" {
		return SendFilePathResult{Err: "peer ref is empty"}
	}
	if path == "" {
		return SendFilePathResult{Err: "path is empty"}
	}
	f, err := os.Open(path)
	if err != nil {
		return SendFilePathResult{Err: fmt.Sprintf("open: %v", err)}
	}
	stat, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return SendFilePathResult{Err: fmt.Sprintf("stat: %v", err)}
	}
	if !stat.Mode().IsRegular() {
		_ = f.Close()
		return SendFilePathResult{Err: "not a regular file: " + path}
	}
	name := filepath.Base(path)
	fileID := newFileID()
	// skipChatLog=true: drag-and-drop now uses a live
	// placeholder bubble (same as the picker route,
	// 2026-06-27). Without this, the user only sees the
	// file card AFTER the transfer finishes — a confusing
	// "I dropped something and nothing happened" gap.
	// The chat.enc record is written unconditionally
	// by Node.SendFile so the bubble survives app restart
	// either way (re-rendered from history on relaunch).
	if err := a.nd.SendFile(peerRef, name, stat.Size(), f, path, fileID, true); err != nil {
		_ = f.Close()
		return SendFilePathResult{Err: err.Error()}
	}
	// f is owned by Node.SendFile's goroutine now; it'll
	// close after the transfer completes. Don't double-close.
	log.Printf("[INFO  app] SendFile (drag-and-drop) peer=%s path=%s size=%d fileID=%s",
		peerRef, path, stat.Size(), fileID)
	return SendFilePathResult{FileID: fileID}
}

// pendingFiles is no longer needed — the picker route
// now uses runtime.OpenFileDialog to get the real path
// directly and SendFilePath to start the transfer. The
// io.Pipe + chunk-streaming API (SendFileOpen/Chunk/Close)
// is gone. See the doc on PickFile for the rationale.

// PickFile opens the native OS file picker via
// wruntime.OpenFileDialog. Returns (path, "") on success;
// ("", "cancelled") if the user dismisses the dialog;
// ("", errMsg) on error.
//
// Why native dialog instead of <input type=file>:
//
//   - <input type=file> runs inside the WebView2 sandbox
//     and deliberately hides the on-disk path for
//     security. core then can't `os.Open` it, can't wire
//     up "right-click → open folder", and has to stream
//     bytes through Wails IPC (which makes progress lag
//     behind the real transfer — "排队中…" for seconds
//     while JS pushes chunks).
//   - Wails v2's runtime.OpenFileDialog is a *native*
//     dialog (Win32 GetOpenFileName / NSOpenPanel /
//     zenity). It hands the real path straight to Go,
//     bypassing the sandbox entirely. No JS round trip,
//     no hidden path, no IPC chunk pipeline.
//
// User reported this issue twice (2026-06-26): the picker
// route was stuck at "排队中…" for many seconds while the
// frontend streamed chunks into core. Switching to the
// native dialog removes the gap entirely — the placeholder
// bubble appears synchronously with the dialog click and
// file:event progress starts within ~100 ms.
// PickFileResult is the (path, errMsg) tuple for
// PickFile returned as a single Go struct. Wails v2 only
// exposes the FIRST return value of a bound Go method
// to TypeScript, so we can't use two-valued returns. A
// struct gets cleanly translated to a TypeScript class
// so the frontend can read both fields without a
// string-parsing hack.
type PickFileResult struct {
	Path string `json:"path"` // "" on cancel/error
	Err  string `json:"err"`  // "" on success; "cancelled" if user dismissed; else real error
}

// PickFile opens the native OS file picker via
// wruntime.OpenFileDialog. Returns (path, "") on success;
// ("", "cancelled") if the user dismisses the dialog;
// ("", errMsg) on error.
//
// Returns PickFileResult (struct, not tuple) for the
// same Wails v2 reason as SendFilePathResult: only the
// first return value is exposed to TypeScript, so we
// can't return (path, errMsg) cleanly. Struct → TS class.
func (a *App) PickFile() PickFileResult {
	if a.ctx == nil {
		return PickFileResult{Err: "wails context not initialised"}
	}
	path, err := wruntime.OpenFileDialog(a.ctx, wruntime.OpenDialogOptions{
		Title: "选择要发送的文件",
		Filters: []wruntime.FileFilter{
			{DisplayName: "All files (*.*)", Pattern: "*.*"},
		},
	})
	if err != nil {
		return PickFileResult{Err: err.Error()}
	}
	if path == "" {
		// User cancelled — frontend treats this as silent.
		return PickFileResult{Err: "cancelled"}
	}
	return PickFileResult{Path: path}
}

// SendFilePathResult is the (fileID, errMsg) tuple for
// SendFilePath returned as a single Go struct. Wails v2
// only exposes the FIRST return value of a bound Go
// method to TypeScript, so we can't use two-valued
// returns. A struct gets cleanly translated to a
// TypeScript class so the frontend can read both
// fields without a string-parsing hack.
type SendFilePathResult struct {
	FileID string `json:"fileID"` // "" on failure
	Err    string `json:"err"`    // "" on success
}

// SendFilePath opens the file at path and starts streaming
// it to peerRef. Returns (fileID, "") on success. fileID
// matches the FileID field in subsequent file:event
// runtime events; the frontend uses it as the bubble key.
//
// This is the picker route's transfer kickoff. It opens
// the file once on the Go side, hands the *os.File to
// Node.SendFile (which then closes it after the transfer),
// and returns immediately. No bytes cross the JS/Go
// boundary on this path — Go reads from disk straight
// into filetransfer.Send which encrypts and ships.
//
// Behaviour matches drag-and-drop (App.SendFile):
//   - skipChatLog=false: a chat message is published and
//     persisted in chat.enc so the bubble survives
//     peer-switch / app-restart.
//   - localPath=path: the sender's right-click "open
//     folder" reveals the user's actual folder.
func (a *App) SendFilePath(peerRef, path string) SendFilePathResult {
	if a.nd == nil {
		return SendFilePathResult{Err: "node not started"}
	}
	if peerRef == "" {
		return SendFilePathResult{Err: "peer ref is empty"}
	}
	if path == "" {
		return SendFilePathResult{Err: "path is empty"}
	}
	f, err := os.Open(path)
	if err != nil {
		return SendFilePathResult{Err: fmt.Sprintf("open: %v", err)}
	}
	stat, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return SendFilePathResult{Err: fmt.Sprintf("stat: %v", err)}
	}
	if !stat.Mode().IsRegular() {
		_ = f.Close()
		return SendFilePathResult{Err: "not a regular file: " + path}
	}
	name := filepath.Base(path)
	fileID := newFileID()
	// skipChatLog=true: the frontend keeps a live
	// placeholder bubble for this fileID in
	// state.fileBubbles, updated in place by the
	// file:event stream. If we also published a chat
	// message here, the frontend would render a SECOND
	// bubble for the same file (one live-progress, one
	// final file://). Node.SendFile now writes the
	// chat.enc record unconditionally so the bubble
	// survives app restart either way (file card
	// re-rendered from history on relaunch).
	if err := a.nd.SendFile(peerRef, name, stat.Size(), f, path, fileID, true); err != nil {
		_ = f.Close()
		return SendFilePathResult{Err: err.Error()}
	}
	// f is owned by Node.SendFile's goroutine now; it'll
	// close after the transfer completes. Don't double-close.
	log.Printf("[INFO  app] SendFilePath peer=%s path=%s size=%d fileID=%s",
		peerRef, path, stat.Size(), fileID)
	return SendFilePathResult{FileID: fileID}
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
// We re-derive that here so anything that writes under
// the data dir lives next to chat.enc / device.key.

// newFileID returns a 16-byte hex fileID. Random, not
// derived from a counter, so two concurrent streams on
// the same path can't accidentally collide. Used as the
// pfID for SendFileOpen (picker route) so the frontend
// doesn't have to invent one.
func newFileID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand on a working OS never fails; if it
		// does, fall back to a nanosecond-derived string
		// so the caller still has a unique-ish key.
		return fmt.Sprintf("local-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func (a *App) dataDir() string {
	// INKL_DATA_DIR env var, if set, takes precedence.
	// Used to run multiple instances side-by-side for
	// local testing (set INKL_DATA_DIR=D:\test1\.innerlink
	// in one launch, D:\test2\.innerlink in another; the
	// two instances get distinct device.key / chat.enc
	// and discover each other over UDP).
	if d := os.Getenv("INKL_DATA_DIR"); d != "" {
		return d
	}
	// Heuristic: walk from cwd. If a .innerlink/ exists here
	// use it; otherwise default to <launch-cwd>/.innerlink/.
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

// DebugReveal opens a known-bad path to verify the
// Explorer / file-manager launch path is working at
// all. Returns a structured report:
//
//	"OK"                       — explorer.exe started
//	"OK:pid=1234"              — pid of the spawned process
//	"FAIL:<error message>"     — could not start explorer
//
// Why this exists: the right-click "打开文件所在文件夹"
// action keeps failing on the user's VM. We need a way
// to verify the Go side is actually launching a
// process without going through the Wails binding +
// frontend click path (which has too many moving parts
// to debug). The frontend exposes a "测试" button that
// calls this with a hard-coded path under
// <data-dir>/received/.
//
// Cross-platform:
//   - Windows: `explorer.exe /select, <path>`
//   - macOS:   `open -R <path>`
//   - Linux:   `xdg-open <parent dir>` (no select in std)
func (a *App) DebugReveal(path string) string {
	if path == "" {
		// Pick a known file under the data dir so the
		// caller can at least confirm the launch path
		// works. <data-dir>/device.key always exists
		// after first launch.
		path = filepath.Join(a.dataDir(), "device.key")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "FAIL:stat " + path + ": " + err.Error()
	}
	// Just spawn; we don't try to verify a new window
	// appeared. explorer.exe on Windows is a
	// singleton-per-user — tasklist always has many
	// explorer processes, and /select re-uses the
	// existing one. Diffing pre/post by name is
	// unreliable; the user can confirm visually.
	if runtime.GOOS == "windows" {
		if err := revealInExplorer(path); err != nil {
			return "FAIL:explorer " + err.Error()
		}
	} else {
		_ = info
		if r := a.RevealInFolder(path); r != "" {
			return "FAIL:reveal " + r
		}
	}
	return "OK:explorer launched (visual check required)"
}

// DebugOpen opens a known path with the default app
// (same as the chat-bubble "open file" action).
func (a *App) DebugOpen(path string) string {
	if path == "" {
		path = filepath.Join(a.dataDir(), "device.key")
	}
	if r := a.OpenPath(path); r != "" {
		return "FAIL:open " + r
	}
	return "OK:open launched (visual check required)"
}

// procSnap is a small snapshot of running processes by
// name. We use it in DebugReveal / DebugOpen to detect
// "did the launch actually spawn a new explorer.exe /
// Finder / etc." without depending on UI feedback.
type procSnap map[string]int // exe name -> pid (any one of)

func snapshotProcs() procSnap {
	snap := make(procSnap)
	if runtime.GOOS != "windows" {
		// ps -A | awk '{print $4}' is portable across
		// macOS and Linux. Windows uses tasklist below.
		out, err := exec.Command("ps", "-A", "-o", "comm=").Output()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				name := strings.TrimSpace(line)
				if name == "" {
					continue
				}
				if _, ok := snap[name]; !ok {
					snap[name] = 0
				}
			}
		}
		return snap
	}
	// Windows: tasklist /FO CSV /NH
	out, err := exec.Command("tasklist", "/FO", "CSV", "/NH").Output()
	if err != nil {
		return snap
	}
	// CSV header is "Image Name","PID","Session Name","Session#","Mem Usage"
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// First field is the image name (quoted).
		if !strings.HasPrefix(line, "\"") {
			continue
		}
		end := strings.Index(line[1:], "\"")
		if end < 0 {
			continue
		}
		name := line[1 : end+1]
		if _, ok := snap[name]; !ok {
			snap[name] = 0
		}
	}
	return snap
}

func diffProcs(pre, post procSnap) []procInfo {
	var out []procInfo
	for name := range post {
		if _, ok := pre[name]; !ok {
			out = append(out, procInfo{Name: name, Pid: post[name]})
		}
	}
	return out
}

type procInfo struct {
	Name string
	Pid  int
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

// GetMyAlias returns our broadcast self-display-name
// ("" if unset). This is the alias that other peers
// see for us, sourced from <data-dir>/alias.txt and
// propagated via M5 RosterSync. The frontend calls
// this once at startup to render the sidebar's "me"
// header.
//
// Distinct from ListAliases (the legacy per-peer
// local-nickname table used by the `alias` REPL
// command): GetMyAlias is the new self-attribute,
// ListAliases is the old per-peer mapping.
func (a *App) GetMyAlias() string {
	if a.nd == nil {
		return ""
	}
	return a.nd.GetSelfAlias()
}

// SetMyAlias sets our broadcast self-display-name.
// Empty string clears it. On success, returns ""
// (no error); on validation failure, returns the
// error message (the user sees this via toast).
//
// Setting the alias triggers an immediate RosterSync
// to every connected peer, so the change is visible
// across the LAN within one round-trip — no need to
// wait for the next gossip cycle.
func (a *App) SetMyAlias(name string) string {
	if a.nd == nil {
		return "node not started"
	}
	if err := a.nd.SetSelfAlias(name); err != nil {
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

// ClearHistory deletes every on-disk chat record for the
// peer identified by peerRef (alias or 32-char hex).
// Returns "" on success, error message on failure.
//
// v1.1 (2026-06-27) makes this a REAL delete — previously
// the GUI "clear chat" button only cleared the in-memory
// display, leaving the per-peer encrypted file untouched.
// The frontend should refresh History() after this returns
// to update the chat panel.
//
// Refuses empty / invalid refs (the storage layer also
// rejects, but we surface a clearer error here).
func (a *App) ClearHistory(peerRef string) string {
	if a.nd == nil {
		return "node not started"
	}
	if peerRef == "" {
		return "peer ref is empty"
	}
	if err := a.nd.DeleteHistory(peerRef); err != nil {
		return err.Error()
	}
	return ""
}

// CancelFile aborts an in-flight outbound file transfer.
// fileID matches the data-file-id on the sender's
// progress bubble. Returns "" on success, error message
// otherwise. Idempotent: cancelling a fileID that's
// already done is a no-op (returns "") — the GUI's ✕
// button can fire late without a spurious toast.
//
// v1.1 (2026-06-27): paired with App.SendFilePath + the
// progress bubble's ✕ button. Closes the 500 MiB
// stuck-transfer footgun (the actual user report that
// drove this fix).
func (a *App) CancelFile(fileID string) string {
	if a.nd == nil {
		return "node not started"
	}
	if fileID == "" {
		return "file id is empty"
	}
	if err := a.nd.CancelFile(fileID); err != nil {
		return err.Error()
	}
	return ""
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

// pumpFiles forwards file-transfer progress + done
// events to the frontend as "file:event" runtime events.
// The frontend subscribes per-fileID and updates the
// matching bubble (progress bar, speed number, ✓/✗).
// Exits when SubscribeFiles' channel is closed.
func (a *App) pumpFiles() {
	log.Printf("[INFO  app] pumpFiles ENTER (goroutines=%d)", runtime.NumGoroutine())
	defer log.Printf("[INFO  app] pumpFiles EXIT (goroutines=%d)", runtime.NumGoroutine())
	for ev := range a.nd.SubscribeFiles() {
		log.Printf("[INFO  app] pumpFiles emit file:event %+v", ev)
		wruntime.EventsEmit(a.ctx, "file:event", ev)
	}
}