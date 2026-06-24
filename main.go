// Package main is the innerlink Wails v2 entry point.
//
// innerlink is a single-binary,国密 (SM2/SM3/SM4) end-to-end
// encrypted P2P communication app for LAN. Same WiFi / same
// NAT, no account, no central server.
//
// This file is intentionally minimal:
//   - assets embed for the Wails frontend
//   - wails.Run with the conventional hooks (Startup / Shutdown)
//   - NO OnBeforeClose, NO PowerShell child-kill, NO Job
//     Object, NO SetWinEventHook watchdog — those are
//     workarounds from earlier prototypes and the Wails v2
//     default quit chain works for this app as long as
//     Shutdown() actually tears down the core goroutines
//     before main returns. See docs/CLOSE-EXIT.md for the
//     reasoning (when we add that doc).
//
// The core protocol layer (UDP/TCP/discovery/SM2/SM3/SM4)
// is implemented in pkg/node + internal/* and is fully
// orthogonal to Wails. cmd/innerlink is a CLI demo that
// exercises the same code without any GUI.
package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/logger"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"

	"github.com/weishengsuptp/innerlink/app"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	a := app.NewApp()

	err := wails.Run(&options.App{
		Title:  "innerlink",
		Width:  1024,
		Height: 720,

		AssetServer: &assetserver.Options{
			Assets: assets,
		},

		// Innerlink palette: warm-neutral surface, same as
		// the v0.x mockup. Avoids a white flash before the
		// HTML loads.
		BackgroundColour: &options.RGBA{R: 0xF7, G: 0xF8, B: 0xF4, A: 1},

		// Wails-level logging: emit everything (DEBUG for
		// window + WebView2 lifecycle, frontend console
		// passthrough). Critical for diagnosing the close
		// bug — Wails' internal quit chain has its own
		// messages we can't otherwise see. NewDefaultLogger
		// writes to stderr.
		Logger:   logger.NewDefaultLogger(),
		LogLevel: logger.DEBUG,

		// Drag-and-drop: enable Wails file-drop event +
		// restrict acceptance to elements marked with
		// `--wails-drop-target: drop` (the .composer-wrap
		// in the frontend). DisableWebViewDrop is false
		// because we still want the webview's default file
		// drop to be off (we handle drops via OnFileDrop).
		DragAndDrop: &options.DragAndDrop{
			EnableFileDrop:     true,
			DisableWebViewDrop: true,
			CSSDropProperty:    "--wails-drop-target",
			CSSDropValue:       "drop",
		},

		OnStartup:    a.Startup,
		OnShutdown:   a.Shutdown,
		OnBeforeClose: a.BeforeClose,
		// OnBeforeClose intentionally NOT set. See top of
		// this file for why (and docs/CLOSE-EXIT.md).
		Bind: []interface{}{a},
	})

	if err != nil {
		log.Fatalf("innerlink: %v", err)
	}
}