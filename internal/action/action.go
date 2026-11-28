// Package action exposes OpenDrain as a Download Action Layer (DAL):
// an agent expresses a download intent and receives a small handle, while all
// state, strategy and integrity decisions stay inside the layer.
package action

import (
	"context"
	"time"

	"opendrain/internal/backend"
	"opendrain/internal/core"
)

// Intent is the only thing an agent needs to express: "I want these bytes."
// Everything else — mirrors, backends, chunking, resume, verification —
// belongs to the layer, not to the caller.
type Intent struct {
	URL      string   `json:"url"`
	Target   string   `json:"target"`
	Checksum string   `json:"checksum,omitempty"` // sha256:<hex>
	Mirrors  []string `json:"mirrors,omitempty"`
	TaskKey  string   `json:"task_key,omitempty"` // stable identity for rotating URLs
	Backend  string   `json:"backend,omitempty"`  // auto | native | aria2
	Resume   bool     `json:"resume"`             // zero value false → default true in Normalize
}

// Normalize fills in DAL defaults. Callers may pass a zero Resume value and
// still get resume-by-default, which is the whole point of a layer that owns
// policy instead of pushing it onto every agent.
func (i Intent) Normalize() Intent {
	if i.Backend == "" {
		i.Backend = "auto"
	}
	i.Resume = true // the layer always resumes unless an explicit override is added
	return i
}

// Handle is the compact result handed back to the agent. Content never
// appears here — only the facts needed to decide what to do next.
type Handle struct {
	ID        string        `json:"id"`
	Target    string        `json:"target"`
	Bytes     int64         `json:"bytes"`
	Sha256    string        `json:"sha256"`
	Integrity string        `json:"integrity"` // checksum | unverified
	Resumed   bool          `json:"resumed"`
	Cached    bool          `json:"cached"`
	Status    string        `json:"status"`
	Warnings  []string      `json:"warnings,omitempty"`
	Error     *core.ErrInfo `json:"error,omitempty"`
}

// OK reports whether the handle points to a complete file at Target.
func (h *Handle) OK() bool { return h != nil && h.Status == core.StatusCompleted && h.Error == nil }

// Layer is the download action layer. It owns state and strategy; harnesses
// only call Act and observe the returned Handle.
type Layer struct {
	backends []core.Downloader
	store    *core.Store
}

// New creates a DAL with the standard backends (native primary, aria2 fallback)
// and the default on-disk state store.
func New() (*Layer, error) {
	store, err := core.NewStore()
	if err != nil {
		return nil, err
	}
	backends := []core.Downloader{backendNewNative(), backendNewAria2()}
	return &Layer{backends: backends, store: store}, nil
}

// Act executes a download intent. It is the single entry point for agents.
func (l *Layer) Act(ctx context.Context, in Intent) *Handle {
	intent := in.Normalize()
	req := core.Request{
		URL:      intent.URL,
		Target:   intent.Target,
		Checksum: intent.Checksum,
		Mirrors:  intent.Mirrors,
		TaskKey:  intent.TaskKey,
		Backend:  intent.Backend,
		Resume:   intent.Resume,
	}
	res, _ := core.Fetch(ctx, req, l.store, l.backends)
	return &Handle{
		ID:        res.ID,
		Target:    res.Target,
		Bytes:     res.Bytes,
		Sha256:    res.Sha256,
		Integrity: res.Integrity,
		Resumed:   res.Resumed,
		Cached:    res.Cached,
		Status:    res.Status,
		Warnings:  res.Warnings,
		Error:     res.Error,
	}
}

// Manifest answers "what did previous sessions leave me" — the DAL's memory.
func (l *Layer) Manifest(verify bool) (*core.Manifest, error) {
	return l.store.Manifest(verify, time.Now())
}

// Verify re-hashes one managed file. Trust is checked, never assumed.
func (l *Layer) Verify(target string) (*core.ManifestEntry, error) {
	return l.store.VerifyTarget(target)
}

// backend constructors are indirected so tests can inject fakes without
// depending on the real aria2c discovery behaviour.
var (
	backendNewNative = func() core.Downloader { return backend.NewNative() }
	backendNewAria2  = func() core.Downloader { return backend.NewAria2() }
)
