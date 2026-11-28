package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
func IDFor(rawURL, target string) string {
	return IDForTask("", rawURL, target)
}

// IDForTask is IDFor with an optional caller-supplied task key. A non-empty
// key wins over the URL entirely, which is the escape hatch for sources whose
// signed URLs rotate faster than the download (HuggingFace/xet models, S3,
// CloudFront). The target stays part of the hash so one key cannot collide
// across two different destination files.
func IDForTask(taskKey, rawURL, target string) string {
	t, err := NormalizeTarget(target)
	if err != nil {
		t = filepath.Clean(target)
	}
	var h [32]byte
	if key := strings.TrimSpace(taskKey); key != "" {
		h = sha256.Sum256([]byte("task:" + key + "\n" + t))
	} else {
		// Presigned GitHub URLs rotate their signature every hour; keying on
		// the raw URL would turn a refreshed link into a brand-new task and
		// throw away resumable bytes. CanonicalURL is also a no-op for hosts
		// it does not recognise.
		h = sha256.Sum256([]byte(CanonicalURL(rawURL) + "\n" + t))
	}
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
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	// State files can contain presigned URLs (short-lived bearer credentials),
	// so keep the directory and files private even if it pre-existed.
	_ = os.Chmod(dir, 0o700)
	return &Store{dir: dir}, nil
}

func (s *Store) Path(id string) string { return filepath.Join(s.dir, id+".json") }

func (s *Store) Dir() string { return s.dir }

// TaskLock blocks until this process holds the exclusive lock for id, or ctx
// is cancelled. Callers must defer the returned release func for the entire
// fetch pipeline — two agents targeting the same file must never write the
// same state file or the same <target>.part concurrently.
func (s *Store) TaskLock(ctx context.Context, id string) (func(), error) {
	return acquireTaskLock(ctx, filepath.Join(s.dir, id+".lock"))
}

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
// leaves a torn state file behind. Use for state transitions (start, finish,
// failure) where durability before returning matters.
func (s *Store) Put(st *State) error {
	b, err := s.marshal(st)
	if err != nil {
		return err
	}
	return s.writeState(st, b, true)
}

// PutProgress persists in-flight progress without fsync. The write still goes
// through tmp + atomic rename, so a crash can never expose a torn file: it
// either sees the previous complete state or the new one. Skipping fsync is
// safe for progress snapshots — on a hard crash the OS page cache still
// contains the write and rename is atomic — and avoids one fsync every 500ms
// during a large download (measured 573ms of pure fsync over a 1GiB fetch).
func (s *Store) PutProgress(st *State) error {
	b, err := s.marshal(st)
	if err != nil {
		return err
	}
	return s.writeState(st, b, false)
}

func (s *Store) marshal(st *State) ([]byte, error) {
	st.UpdatedAt = time.Now()
	return json.MarshalIndent(st, "", "  ")
}

func (s *Store) writeState(st *State, b []byte, sync bool) error {
	tmp := s.Path(st.ID) + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	// The mode argument above only applies at creation; a state file created
	// by an older version is still 0644, so tighten it on every write.
	_ = f.Chmod(0o600)
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if sync {
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path(st.ID))
}
