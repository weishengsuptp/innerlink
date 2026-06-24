package node

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// selfAliasMaxLen caps the user-typed alias at 64 bytes
// (UTF-8). Anything longer is almost certainly a paste
// error — same cap as the legacy internal/alias package.
const selfAliasMaxLen = 64

// selfAliasFile is the conventional on-disk path for
// our broadcast alias: <DataDir>/alias.txt. We use a
// single string rather than a JSON object because the
// payload is literally one field; a JSON wrapper would
// be ceremony for no benefit. The file is plain UTF-8,
// no BOM, trailing newline omitted (trimmed on load).
//
// Self-alias is **conceptually distinct** from the
// legacy aliases.json table (internal/alias):
//
//   - aliases.json : I name OTHERS  ("alias <name> <peer>"
//     in the REPL). Per-receiver, local-only.
//   - alias.txt    : I name MYSELF  ("how I want other
//     peers to call me"). Per-sender, broadcast via M5
//     RosterSync.
//
// Both can coexist; they answer different questions.
// The GUI uses alias.txt exclusively.
const selfAliasFile = "alias.txt"

// selfAliasStore is the in-memory + on-disk cache for
// our broadcast self-alias. All exported methods are
// safe for concurrent use.
type selfAliasStore struct {
	path string

	mu      sync.RWMutex
	current string

	saveMu sync.Mutex // serializes disk writes
	dirty  bool
}

// loadSelfAlias reads the alias from path. An empty
// file or a non-existent file both yield "" (no
// alias set yet). Whitespace is trimmed; trailing
// newlines from text editors don't trip up parsing.
//
// A corrupt file (e.g. a binary blob that can't be
// read as UTF-8 text) is a hard error — the same
// "no silent data loss" policy as the legacy alias
// and roster stores.
func loadSelfAlias(path string) (*selfAliasStore, error) {
	s := &selfAliasStore{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, fmt.Errorf("selfalias: read %s: %w", path, err)
	}
	// Treat empty file as "".
	if len(data) == 0 {
		return s, nil
	}
	s.current = strings.TrimSpace(string(data))
	return s, nil
}

// Set updates the in-memory alias and returns whether
// it actually changed (false = same value, no disk
// write needed). Empty string clears the alias.
//
// Validation:
//   - length 0..selfAliasMaxLen bytes (UTF-8 trimmed)
//   - no newlines (the file is one line)
//
// ErrEmptyName is returned only as a sentinel — callers
// in node.go translate it to a user-facing log line.
var ErrEmptyName = errors.New("selfalias: name must not be empty")
var ErrNameTooLong = errors.New("selfalias: name must be <= 64 bytes")
var ErrNameHasNewline = errors.New("selfalias: name must not contain newline")

func (s *selfAliasStore) Set(name string) (changed bool, err error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		// Allow clearing.
	} else {
		if len(trimmed) > selfAliasMaxLen {
			return false, ErrNameTooLong
		}
		if strings.ContainsAny(trimmed, "\r\n") {
			return false, ErrNameHasNewline
		}
	}
	s.mu.Lock()
	if s.current == trimmed {
		s.mu.Unlock()
		return false, nil
	}
	s.current = trimmed
	s.dirty = true
	s.mu.Unlock()
	return true, nil
}

// Get returns the current alias ("" if none).
func (s *selfAliasStore) Get() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

// Save writes the in-memory alias to disk atomically
// (tmp file + rename). No-op if no changes are pending.
// The map is COPIED under the read lock before the
// JSON marshal — same pattern as internal/roster.Save
// after the macOS arm64 race-detector caught a
// concurrent-write bug there.
func (s *selfAliasStore) Save() error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	s.mu.RLock()
	dirty := s.dirty
	current := s.current
	s.mu.RUnlock()
	if !dirty {
		return nil
	}

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, "alias-*.txt.tmp")
	if err != nil {
		return fmt.Errorf("selfalias: create tmp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		// Best-effort cleanup on any error path.
		_ = os.Remove(tmpName)
	}()
	if _, err := tmp.WriteString(current); err != nil {
		tmp.Close()
		return fmt.Errorf("selfalias: write tmp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("selfalias: close tmp: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("selfalias: rename: %w", err)
	}
	s.mu.Lock()
	s.dirty = false
	s.mu.Unlock()
	return nil
}