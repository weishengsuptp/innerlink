//go:build windows

package app

import (
	"os/exec"
	"syscall"
)

// setSysProcAttr hides the spawned console window on
// Windows. We use `cmd /c start ""` and `explorer.exe`
// to open files / reveal-in-folder; both briefly create
// a console window without this. On other OSes the
// equivalent is a no-op (see app_open_others.go).
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
