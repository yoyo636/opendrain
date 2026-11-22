package core

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ManifestEntry is one file OpenDrain manages on disk. This is what an agent
// reads at session start to recover "what did the previous session leave me".
type ManifestEntry struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	Target    string    `json:"target"`
	Status    string    `json:"status"`
	Bytes     int64     `json:"bytes,omitempty"`
	Sha256    string    `json:"sha256,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	OnDisk    bool      `json:"on_disk"`
	Verified  *bool     `json:"verified,omitempty"`
	Note      string    `json:"note,omitempty"`
}

// Manifest is the session-level view of all managed files.
type Manifest struct {
	OK         bool            `json:"ok"`
	Count      int             `json:"count"`
	OnDisk     int             `json:"on_disk"`
	Entries    []ManifestEntry `json:"entries"`
	DurationMS int64           `json:"duration_ms"`
}

// Manifest scans every state file and reports the files OpenDrain manages.
// With verify=true, each file still on disk is re-hashed and compared against
// the recorded digest — so an agent can know which bytes it can trust before
// it starts working, instead of assuming a cache hit means a good file.
func (s *Store) Manifest(verify bool, begin time.Time) (*Manifest, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	m := &Manifest{OK: true, Entries: []ManifestEntry{}}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".json.corrupt") {
			continue
		}
		id := strings.TrimSuffix(name, ".json")
		st, gerr := s.Get(id)
		if gerr != nil || st == nil {
			continue
		}
		target := st.Request.Target
		ent := ManifestEntry{
			ID: id, URL: st.Request.URL, Target: target,
			Status: st.Status, Bytes: st.Total, Sha256: st.Sha256,
			UpdatedAt: st.UpdatedAt,
		}
		if fi, serr := os.Stat(target); serr == nil && !fi.IsDir() && fi.Size() > 0 {
			ent.OnDisk = true
			if ent.Bytes <= 0 {
				ent.Bytes = fi.Size()
			}
			if verify && ent.Sha256 != "" {
				sum, herr := FileSha256(target)
				ok := herr == nil && ent.Sha256 == "sha256:"+sum
				ent.Verified = &ok
				if !ok {
					ent.Note = "digest mismatch; re-fetch before trusting this file"
				}
			}
		} else {
			ent.Note = "target missing; re-fetch to restore"
		}
		m.Entries = append(m.Entries, ent)
	}
	sort.Slice(m.Entries, func(i, j int) bool {
		return m.Entries[i].UpdatedAt.After(m.Entries[j].UpdatedAt)
	})
	m.Count = len(m.Entries)
	for _, e := range m.Entries {
		if e.OnDisk {
			m.OnDisk++
		}
	}
	m.DurationMS = time.Since(begin).Milliseconds()
	return m, nil
}

// FindByTarget locates a managed file by its absolute target path. Used by
// `verify --target` and `read` so the agent can refer to files by path alone.
func (s *Store) FindByTarget(target string) (*State, error) {
	abs, err := filepath.Abs(target)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".json.corrupt") {
			continue
		}
		st, gerr := s.Get(strings.TrimSuffix(name, ".json"))
		if gerr != nil || st == nil {
			continue
		}
		t, terr := filepath.Abs(st.Request.Target)
		if terr == nil && t == abs {
			return st, nil
		}
	}
	return nil, nil
}
