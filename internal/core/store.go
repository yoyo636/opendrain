package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// StateDir defaults to ~/.opendrain/state; OPENDRAIN_STATE_DIR overrides it
// (used by tests). Download state never lives in memory alone.
func StateDir() string {
	if v := os.Getenv("OPENDRAIN_STATE_DIR"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".opendrain-state"
	}
	return filepath.Join(home, ".opendrain", "state")
}

// NormalizeTarget resolves a target path to the absolute, cleaned form that
// every component must agree on. The same logical target must map to the
// same state id whether it was given relative or absolute — otherwise
// `status --target relative/path` cannot find the state that
// `fetch --target relative/path` wrote.
func NormalizeTarget(target string) (string, error) {
	if target == "" {
		return "", nil
	}
	return filepath.Abs(target)
}

// IDFor keys a download by url+target: re-calling with the same arguments
// finds the same state file and resumes.
func IDFor(url, target string) string {
	t, err := NormalizeTarget(target)
	if err != nil {
		t = filepath.Clean(target)
	}
	h := sha256.Sum256([]byte(url + "\n" + t))
	return hex.EncodeToString(h[:])
}

// ErrCorruptState marks a state file that exists but cannot be parsed. The
// file carries no usable progress (it may even be the only record of the
// chunk map), so callers should recover by starting over rather than
// failing forever.
var ErrCorruptState = errors.New("state file is corrupt")

type Store struct{ dir string }

func NewStore() (*Store, error) {
	dir := StateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

func (s *Store) Path(id string) string { return filepath.Join(s.dir, id+".json") }

func (s *Store) Dir() string { return s.dir }

// Get returns (nil, nil) when no state exists for id.
func (s *Store) Get(id string) (*State, error) {
	b, err := os.ReadFile(s.Path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("%w: %v (file: %s)", ErrCorruptState, err, s.Path(id))
	}
	return &st, nil
}

// Quarantine moves a broken state file aside instead of deleting it, so the
// evidence survives while the task is no longer blocked by it.
func (s *Store) Quarantine(id string) error {
	src := s.Path(id)
	if _, err := os.Stat(src); err != nil {
		return err
	}
	return os.Rename(src, src+".corrupt")
}

// Put persists the state atomically (tmp + fsync + rename) so a crash never
// leaves a torn state file behind.
func (s *Store) Put(st *State) error {
	st.UpdatedAt = time.Now()
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.Path(st.ID) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path(st.ID))
}
