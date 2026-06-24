//go:build windows

package app

import "os/exec"

// setSysProcAttr is intentionally a no-op on Windows.
//
// The previous version set SysProcAttr.HideWindow = true
// for every spawned process, on the theory that the
// brief console window from `cmd /c start` or
// `rundll32.exe url.dll,FileProtocolHandler` was
// annoying. It IS annoying for those, but it ALSO
// hides the GUI window of `explorer.exe /select,<path>`
// — which is the "open in folder" action the user is
// actually using. The result: explorer.exe launches,
// loads the file metadata, opens a window — but the
// window starts in the SW_HIDE state, so the user sees
// a spinning busy cursor and no folder window pops up.
//
// The actual flash from rundll32 is so brief (<50ms)
// it's effectively invisible. The flash from `cmd /c
// start "" <path>` can be hidden differently (we use
// rundll32 now, so the cmd path is unused). The
// explorer hiding was the only lasting effect of
// setSysProcAttr, and it was a regression.
//
// If we ever need to suppress a console window again,
// the right tool is CreationFlags = CREATE_NO_WINDOW
// (0x08000000), scoped to the specific command. Not a
// blanket HideWindow on every spawn.
func setSysProcAttr(cmd *exec.Cmd) {}
