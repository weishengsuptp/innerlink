package filetransfer

import (
	"os"
	"path/filepath"
	"testing"
)

// uniquePath (receiver side) produces a non-existing
// path in saveDir. It mirrors the same "name (N).ext"
// convention used by the GUI's picker route so that
// direct-save on collision is predictable across both
// sides.
func TestUniquePath(t *testing.T) {
	dir := t.TempDir()

	// Free -> returned as-is.
	got := uniquePath(dir, "report.pdf")
	if want := filepath.Join(dir, "report.pdf"); got != want {
		t.Errorf("free: got %q, want %q", got, want)
	}

	// One taken.
	if err := os.WriteFile(filepath.Join(dir, "report.pdf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got = uniquePath(dir, "report.pdf")
	if want := filepath.Join(dir, "report (1).pdf"); got != want {
		t.Errorf("one taken: got %q, want %q", got, want)
	}

	// Two taken -> (2).pdf.
	if err := os.WriteFile(filepath.Join(dir, "report (1).pdf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got = uniquePath(dir, "report.pdf")
	if want := filepath.Join(dir, "report (2).pdf"); got != want {
		t.Errorf("two taken: got %q, want %q", got, want)
	}

	// No extension: stem is the whole name, ext is "".
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	got = uniquePath(dir, "README")
	if want := filepath.Join(dir, "README (1)"); got != want {
		t.Errorf("no ext: got %q, want %q", got, want)
	}
}
