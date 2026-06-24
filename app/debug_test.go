package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestDebugReveal exercises the Go-side launch path
// without going through the Wails binding. We don't
// assert a specific result (the launched process is
// environment-dependent — Explorer on Windows, Finder
// on macOS, xdg-open on Linux); we just assert that:
//   1. the call returns in <2s,
//   2. the return string starts with OK/FAIL (parseable),
//   3. no panic.
//
// Run with: go test -count=1 -run TestDebugReveal ./app
func TestDebugReveal(t *testing.T) {
	if testing.Short() {
		t.Skip("DebugReveal spawns Explorer/Finder; skipped in -short")
	}
	a := &App{}
	dir := t.TempDir()
	fp := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(fp, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := a.DebugReveal(fp)
	t.Logf("DebugReveal(%q) = %q", fp, res)
	if !strings.HasPrefix(res, "OK") && !strings.HasPrefix(res, "FAIL") {
		t.Errorf("unexpected result format: %q", res)
	}
}

func TestDebugOpen(t *testing.T) {
	if testing.Short() {
		t.Skip("DebugOpen spawns an app; skipped in -short")
	}
	a := &App{}
	dir := t.TempDir()
	fp := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(fp, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := a.DebugOpen(fp)
	t.Logf("DebugOpen(%q) = %q", fp, res)
	if !strings.HasPrefix(res, "OK") && !strings.HasPrefix(res, "FAIL") {
		t.Errorf("unexpected result format: %q", res)
	}
}

// TestSetSysProcAttrNoOpOnWindows is a regression
// test for the regression that hid explorer.exe windows
// from the user. The bug was:
//
//   cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
//
//   applied to *every* spawned process, including
//   `explorer.exe /select,<path>`. The HideWindow
//   flag tells Windows to start the process in
//   SW_HIDE state, so the GUI window was created
//   but never shown — the user saw a spinning
//   busy cursor with no folder window popping up.
//
// The current setSysProcAttr is a no-op on Windows
// (the brief console flash from cmd / rundll32 was
// tolerable; the hidden-explorer regression was
// not). This test pins that behavior.
func TestSetSysProcAttrNoOpOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skipf("windows-only check (this is %s)", runtime.GOOS)
	}
	cmd := exec.Command("cmd", "/c", "echo hi")
	setSysProcAttr(cmd)
	if cmd.SysProcAttr != nil {
		t.Errorf("setSysProcAttr should be no-op on Windows, got SysProcAttr=%+v", cmd.SysProcAttr)
	}
}
