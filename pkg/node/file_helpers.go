// Small helpers used by the receive-side file plumbing
// that don't fit cleanly into one of the existing files.
// v1.1 (2026-06-28): moveFileCrossDev for the group file
// OnComplete path — files are first written to the
// default <dataDir>/received/ tree (because the per-peer
// Receiver is constructed once with that saveDir), then
// moved to per-group directories on completion. os.Rename
// is the fast path (atomic on the same filesystem) but
// fails with EXDEV when source and target live on
// different volumes; the fallback copies + removes.

package node

import (
	"fmt"
	"io"
	"os"
)

// moveFileCrossDev moves src to dst even when they live
// on different filesystems (os.Rename's EXDEV case). It
// streams via io.Copy so it works for files of any size;
// not as fast as os.Rename for the same-FS case (caller
// tries Rename first), but still avoids loading the
// whole file into memory.
func moveFileCrossDev(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open src: %w", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open dst: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return fmt.Errorf("copy: %w", err)
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return fmt.Errorf("close dst: %w", err)
	}
	if err := os.Remove(src); err != nil {
		return fmt.Errorf("remove src: %w", err)
	}
	return nil
}
