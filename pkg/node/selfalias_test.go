package node

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSelfAlias_LoadMissingFile: a non-existent alias.txt
// yields an empty store, not an error. Same policy as
// alias / roster / storage — no side effects until the
// user actually sets something.
func TestSelfAlias_LoadMissingFile(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "alias.txt")
	s, err := loadSelfAlias(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Get(); got != "" {
		t.Errorf("fresh store has alias = %q, want empty", got)
	}
}

// TestSelfAlias_LoadEmptyFile: zero-byte file also
// yields "".
func TestSelfAlias_LoadEmptyFile(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "alias.txt")
	if err := writeFile(t, tmp, ""); err != nil {
		t.Fatal(err)
	}
	s, err := loadSelfAlias(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Get(); got != "" {
		t.Errorf("empty-file load: alias = %q, want empty", got)
	}
}

// TestSelfAlias_RoundTrip: write, close, re-load, get
// the same string back. Trailing newline (from a text
// editor) is trimmed.
func TestSelfAlias_RoundTrip(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "alias.txt")
	s, err := loadSelfAlias(tmp)
	if err != nil {
		t.Fatal(err)
	}

	changed, err := s.Set("老板")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("first Set: changed=false, want true")
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	s2, err := loadSelfAlias(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.Get(); got != "老板" {
		t.Errorf("after round-trip: alias = %q, want 老板", got)
	}
}

// TestSelfAlias_SetIdempotent: setting the same value
// twice returns changed=false on the second call (no
// disk write scheduled). Saving after that is a no-op.
func TestSelfAlias_SetIdempotent(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "alias.txt")
	s, _ := loadSelfAlias(tmp)

	if changed, _ := s.Set("Alice"); !changed {
		t.Error("first Set: changed=false, want true")
	}
	if changed, _ := s.Set("Alice"); changed {
		t.Error("second Set (same value): changed=true, want false")
	}
	if changed, _ := s.Set(""); !changed {
		t.Error("clear Set: changed=false, want true")
	}
}

// TestSelfAlias_Validation: rejects newlines, rejects
// overlong names. Empty is allowed (clears the alias).
func TestSelfAlias_Validation(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "alias.txt")
	s, _ := loadSelfAlias(tmp)

	if _, err := s.Set("line1\nline2"); err != ErrNameHasNewline {
		t.Errorf("newline: err = %v, want ErrNameHasNewline", err)
	}
	if _, err := s.Set("line1\rline2"); err != ErrNameHasNewline {
		t.Errorf("carriage return: err = %v, want ErrNameHasNewline", err)
	}
	if _, err := s.Set(strings.Repeat("x", 65)); err != ErrNameTooLong {
		t.Errorf("65-char name: err = %v, want ErrNameTooLong", err)
	}
	// Empty is the clear path — must NOT error.
	if _, err := s.Set(""); err != nil {
		t.Errorf("empty (clear): err = %v, want nil", err)
	}
	// Trimmed empty is also clear.
	if _, err := s.Set("   \t  "); err != nil {
		t.Errorf("whitespace-only: err = %v, want nil", err)
	}
}

// TestSelfAlias_PersistenceSafe: a Save with no
// pending changes is a no-op (doesn't touch the file,
// doesn't error). This matters because Node.Close()
// calls Save() unconditionally as a flush, and we
// don't want every shutdown to rewrite alias.txt.
func TestSelfAlias_PersistenceSafe(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "alias.txt")
	s, _ := loadSelfAlias(tmp)
	// Set + Save once so the file exists.
	s.Set("Alice")
	s.Save()
	// Second Save with no changes is a no-op.
	if err := s.Save(); err != nil {
		t.Errorf("idempotent Save: err = %v", err)
	}
}

// writeFile is a tiny helper so the empty-file test
// doesn't pull in os.WriteFile at the top of the
// file (Go's std lib doesn't have a public helper
// for "create or truncate" that's simpler).
func writeFile(t *testing.T, path, contents string) error {
	t.Helper()
	return os.WriteFile(path, []byte(contents), 0o600)
}