package app

import (
	"os"
	"path/filepath"
	"testing"
)

// uniquePath returns the first non-existing name in
// dir. We test the three branches:
//
//  1. Name free -> returned as-is.
//  2. Name taken -> "<stem> (1).<ext>".
//  3. Two taken -> "<stem> (2).<ext>" (skips (1) only
//     if (1) is also taken).
func TestUniquePath(t *testing.T) {
	dir := t.TempDir()

	// 1) free
	got := uniquePath(dir, "foo.txt")
	want := filepath.Join(dir, "foo.txt")
	if got != want {
		t.Errorf("free: got %q, want %q", got, want)
	}

	// 2) one taken
	if err := os.WriteFile(filepath.Join(dir, "foo.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got = uniquePath(dir, "foo.txt")
	want = filepath.Join(dir, "foo (1).txt")
	if got != want {
		t.Errorf("one taken: got %q, want %q", got, want)
	}

	// 3) two taken (foo.txt and foo (1).txt both exist).
	// uniquePath should return "foo (2).txt".
	if err := os.WriteFile(filepath.Join(dir, "foo (1).txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got = uniquePath(dir, "foo.txt")
	want = filepath.Join(dir, "foo (2).txt")
	if got != want {
		t.Errorf("two taken: got %q, want %q", got, want)
	}

	// 4) no extension
	got = uniquePath(dir, "README")
	want = filepath.Join(dir, "README")
	if got != want {
		t.Errorf("no ext free: got %q, want %q", got, want)
	}
	if err := os.WriteFile(filepath.Join(dir, "README"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got = uniquePath(dir, "README")
	want = filepath.Join(dir, "README (1)")
	if got != want {
		t.Errorf("no ext taken: got %q, want %q", got, want)
	}
}
