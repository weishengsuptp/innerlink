//go:build !windows

package app

import "os/exec"

// setSysProcAttr is a no-op on non-Windows platforms.
// `syscall.SysProcAttr` doesn't have a `HideWindow` field
// on Linux/macOS (it's a Windows-only concept), and we
// don't need it there: spawning `xdg-open` / `open`
// doesn't show a console window to begin with.
func setSysProcAttr(cmd *exec.Cmd) {}
