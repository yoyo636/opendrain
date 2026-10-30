package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// Downloader is implemented by every backend (aria2, native). Start blocks
// until the part file is complete, failed, or cancelled; progress is written
// to the store continuously so any interruption is resumable.
type Downloader interface {
	Name() string
	Available(ctx context.Context) bool
	Start(ctx context.Context, st *State, store *Store) error
	Status(id string) (*State, error)
	Cancel(id string) error
}

// Result is the single JSON line returned to the agent on stdout.
type Result struct {
	OK         bool        `json:"ok"`
	Status     string      `json:"status"`
	ID         string      `json:"id,omitempty"`
	Target     string      `json:"target,omitempty"`
	Bytes      int64       `json:"bytes,omitempty"`
	Sha256     string      `json:"sha256,omitempty"`
	Backend    string      `json:"backend,omitempty"`
	Selected   string      `json:"selected_mirror,omitempty"`
	Mirrors    []ProbeInfo `json:"mirrors,omitempty"`
	Resumed    bool        `json:"resumed,omitempty"`
	Cached     bool        `json:"cached,omitempty"`
	Warnings   []string    `json:"warnings,omitempty"`
	DurationMS int64       `json:"duration_ms"`
	Error      *ErrInfo    `json:"error,omitempty"`
}

// Fetch runs the full pipeline: validate → probe mirrors → pick backend →
// download into <target>.part → verify checksum → atomically publish.
// The target path only ever holds fully downloaded, checksum-verified bytes.
func Fetch(ctx context.Context, req Request, store *Store, backends []Downloader) (*Result, *State) {
	begin := time.Now()
	res := &Result{}

	if u, err := url.Parse(req.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fatal(store, nil, CodeBadInput, "url must be an http(s) URL", nil, begin)
	}
	if req.Target == "" {
		return fatal(store, nil, CodeBadInput, "target path is required", nil, begin)
	}
	abs, err := filepath.Abs(req.Target)
	if err != nil {
		return fatal(store, nil, CodeBadInput, "bad target path: "+err.Error(), nil, begin)
	}
	req.Target = abs
	if fi, err := os.Stat(req.Target); err == nil && fi.IsDir() {
		return fatal(store, nil, CodeBadInput, "target is an existing directory: "+req.Target, nil, begin)
	}
	if req.Checksum != "" {
		algo, hexv, cerr := ParseChecksum(req.Checksum)
		if cerr != nil {
			return fatal(store, nil, CodeBadInput, cerr.Error(), nil, begin)
		}
		req.Checksum = algo + ":" + hexv
	}
	if err := os.MkdirAll(filepath.Dir(req.Target), 0o755); err != nil {
		return fatal(store, nil, CodeBadInput, "cannot create target dir: "+err.Error(), nil, begin)
	}
	if req.Backend == "" {
		req.Backend = "auto"
	}

	id := IDFor(req.URL, req.Target)
	res.ID = id
	st, err := store.Get(id)
	if err != nil {
		if !errors.Is(err, ErrCorruptState) {
			return fatal(store, nil, CodeInternal, "read state: "+err.Error(), nil, begin)
		}
		// A corrupt state file holds no usable progress — it may even be the
		// only record of which chunks were already on disk. Failing here
		// would make the task permanently unrunnable: the agent would keep
		// re-issuing the same command and getting the same error forever.
		// Quarantine the broken file and start over honestly (resumed=false).
		st = &State{ID: id, Status: StatusNew, CreatedAt: time.Now()}
		st.Warnings = []string{"state file was corrupt and has been reset: " + err.Error()}
		_ = store.Quarantine(id)
	}
	if st == nil {
		st = &State{ID: id, Status: StatusNew, CreatedAt: time.Now()}
	}
	st.Request = req
	st.PartPath = req.Target + ".part"
	st.Error = nil
	res.Target = req.Target

	resumed := false

	// Fast path: already completed and the file is still there — re-hash so
	// the agent always gets a digest of the bytes actually on disk.
	if st.Status == StatusCompleted {
		if fi, serr := os.Stat(req.Target); serr == nil && fi.Size() > 0 {
			sum, herr := FileSha256(req.Target)
			if herr == nil {
				st.Sha256 = "sha256:" + sum
				st.Total = fi.Size()
				if req.Checksum != "" && st.Sha256 != req.Checksum {
					os.Remove(req.Target)
					return fatal(store, st, CodeChecksum,
						"checksum mismatch on cached file; file removed",
						map[string]any{"expected": req.Checksum, "actual": st.Sha256}, begin)
				}
				_ = store.Put(st)
				fillResult(res, st, false, true, begin)
				return res, st
			}
		}
		st.Status = StatusNew // file is gone; download again
	}

	if !req.Resume {
		os.Remove(st.PartPath)
		os.Remove(st.PartPath + ".aria2")
		st.Chunks, st.Done, st.Mode, st.Total, st.Sha256 = nil, 0, "", 0, ""
		st.Status = StatusNew
	} else if st.Mode != "" || len(st.Chunks) > 0 || st.Done > 0 {
		// Progress only survives if the part file is still there.
		if _, perr := os.Stat(st.PartPath); perr != nil {
			st.Chunks, st.Done, st.Mode, st.Total = nil, 0, "", 0
		}
	}

	// `resumed` is computed here, AFTER the progress reset above: if the
	// part file was gone the recorded progress was dropped and the download
	// restarts from zero, so claiming "resumed" would be a lie. The agent
	// uses this flag to decide whether a re-call was cheap.
	if req.Resume && (st.Status == StatusDownloading || st.Status == StatusFailed || st.Status == StatusCancelled) {
		if st.Done > 0 || len(st.Chunks) > 0 {
			resumed = true
		}
	}

	// Probe mirrors.
	st.Status = StatusProbing
	_ = store.Put(st)
	allURLs := append([]string{req.URL}, req.Mirrors...)
	probes := ProbeAll(ctx, allURLs, 6*time.Second)
	st.Probes = probes
	cands := CandidateURLs(st)
	if len(cands) == 0 {
		return fatal(store, st, CodeNoMirror, "no mirror responded",
			map[string]any{"probes": probes}, begin)
	}
	st.Selected = cands[0]

	// Pick backend: aria2 preferred (mature multi-connection), native fallback.
	var chosen, fallback Downloader
	byName := func(name string) Downloader {
		for _, b := range backends {
			if b.Name() == name {
				return b
			}
		}
		return nil
	}
	switch req.Backend {
	case "aria2":
		if b := byName("aria2"); b != nil && b.Available(ctx) {
			chosen = b
		} else {
			return fatal(store, st, CodeBadInput, "backend aria2 unavailable (aria2c not found)", nil, begin)
		}
	case "native":
		chosen = byName("native")
	default: // auto
		// aria2 is preferred for big files (mature multi-connection); for
		// small ones its process+RPC overhead dwarfs the transfer, and
		// parallel connections buy nothing below the ranged threshold.
		sz := int64(-1)
		for _, p := range st.Probes {
			if p.Error == "" {
				sz = p.Size
				break
			}
		}
		if b := byName("aria2"); b != nil && b.Available(ctx) && (sz < 0 || sz >= 8<<20) {
			chosen = b
		}
		fallback = byName("native")
		if chosen == nil {
			chosen, fallback = fallback, nil
		}
	}
	if chosen == nil {
		return fatal(store, st, CodeInternal, "no downloader backend available", nil, begin)
	}

	restarted := false
	run := func(b Downloader) error {
		// Backends use incompatible partial-file formats (native: sparse file
		// + our chunk map; aria2: its own .aria2 control file and "existing
		// bytes are a downloaded prefix" semantics). Switching backends — or
		// resuming aria2 without its control file — invalidates the part
		// bytes, so discard them instead of letting them corrupt the file.
		hasProgress := st.Done > 0 || len(st.Chunks) > 0 || st.Mode != ""
		if hasProgress && b.Name() == "aria2" {
			if _, err := os.Stat(st.PartPath + ".aria2"); err != nil {
				_ = os.Truncate(st.PartPath, 0)
				st.Chunks, st.Done, st.Mode = nil, 0, ""
				restarted = true
			}
		} else if hasProgress && b.Name() == "native" && st.Backend == "aria2" {
			_ = os.Truncate(st.PartPath, 0)
			st.Chunks, st.Done, st.Mode = nil, 0, ""
			restarted = true
		}
		st.Backend = b.Name()
		st.Status = StatusDownloading
		st.Error = nil
		_ = store.Put(st)
		return b.Start(ctx, st, store)
	}
	berr := run(chosen)
	if berr != nil && fallback != nil {
		st.Attempts++
		berr = run(fallback)
	}
	if berr != nil {
		// Pull the freshest progress from disk before stamping the failure,
		// so chunk positions saved by the backend aren't overwritten.
		if fresh, gerr := store.Get(id); gerr == nil && fresh != nil {
			keepCreated := st.CreatedAt
			*st = *fresh
			st.CreatedAt = keepCreated
		}
		code, status := CodeInternal, StatusFailed
		var ce *Error
		if errors.As(berr, &ce) {
			code = ce.Info.Code
			if code == CodeCancelled {
				status = StatusCancelled
			} else {
				st.Error = &ce.Info
			}
		}
		if st.Error == nil {
			st.Error = &ErrInfo{Code: code, Message: berr.Error()}
		}
		st.Status = status
		_ = store.Put(st)
		res.OK = false
		res.Status = status
		res.Backend = st.Backend
		res.Mirrors = st.Probes
		res.Selected = st.Selected
		res.Error = st.Error
		res.DurationMS = time.Since(begin).Milliseconds()
		return res, st
	}

	// Verify, then publish atomically.
	st.Status = StatusVerifying
	_ = store.Put(st)
	if fi, err := os.Stat(st.PartPath); err == nil && st.Total <= 0 {
		st.Total = fi.Size()
	}
	sum, err := FileSha256(st.PartPath)
	if err != nil {
		return fatal(store, st, CodeInternal, "hash part file: "+err.Error(), nil, begin)
	}
	st.Sha256 = "sha256:" + sum
	if req.Checksum != "" && st.Sha256 != req.Checksum {
		os.Remove(st.PartPath)
		return fatal(store, st, CodeChecksum, "checksum mismatch; partial file removed",
			map[string]any{"expected": req.Checksum, "actual": st.Sha256}, begin)
	}
	if st.Total > 0 {
		if fi, serr := os.Stat(st.PartPath); serr != nil || fi.Size() != st.Total {
			return fatal(store, st, CodeIncomplete,
				fmt.Sprintf("size mismatch: file %d, expected %d", sizeOf(st.PartPath), st.Total), nil, begin)
		}
	}
	st.Done = st.Total
	if err := os.Rename(st.PartPath, req.Target); err != nil {
		return fatal(store, st, CodeInternal, "publish file: "+err.Error(), nil, begin)
	}
	st.Status = StatusCompleted
	// The reset notice has been delivered by this run; drop it from the
	// persisted state so later cached fetches do not keep repeating it.
	reported := st.Warnings
	st.Warnings = nil
	_ = store.Put(st)
	fillResult(res, st, resumed && !restarted, false, begin)
	res.Warnings = reported
	return res, st
}

func sizeOf(p string) int64 {
	if fi, err := os.Stat(p); err == nil {
		return fi.Size()
	}
	return -1
}

func fatal(store *Store, st *State, code, msg string, detail map[string]any, begin time.Time) (*Result, *State) {
	res := &Result{OK: false, Status: StatusFailed, DurationMS: time.Since(begin).Milliseconds()}
	if st != nil {
		st.Status = StatusFailed
		st.Error = &ErrInfo{Code: code, Message: msg, Detail: detail}
		_ = store.Put(st)
		res.ID = st.ID
		res.Target = st.Request.Target
		res.Backend = st.Backend
		res.Mirrors = st.Probes
		res.Warnings = st.Warnings
	}
	res.Error = &ErrInfo{Code: code, Message: msg, Detail: detail}
	return res, st
}

func fillResult(res *Result, st *State, resumed, cached bool, begin time.Time) {
	res.OK = st.Status == StatusCompleted
	res.Status = st.Status
	res.Target = st.Request.Target
	res.Bytes = st.Total
	res.Sha256 = st.Sha256
	res.Backend = st.Backend
	res.Selected = st.Selected
	res.Mirrors = st.Probes
	res.Resumed = resumed
	res.Cached = cached
	res.Warnings = st.Warnings
	res.DurationMS = time.Since(begin).Milliseconds()
	res.Error = st.Error
}

// StateJSON is a convenience for the status subcommand.
func StateJSON(st *State) []byte {
	b, _ := json.MarshalIndent(st, "", "  ")
	return b
}
