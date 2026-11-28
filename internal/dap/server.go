package dap

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"opendrain/internal/action"
	"opendrain/internal/core"
)

// Server wires the protocol dispatcher to a real action.Layer and implements
// the optional clean/read methods using the layer's store.
type Server struct {
	engine  *action.Layer
	store   *core.Store
	handler *Dispatcher
}

// NewServer creates a protocol server backed by the reference implementation.
func NewServer() (*Server, error) {
	l, err := action.New()
	if err != nil {
		return nil, err
	}
	store, err := core.NewStore()
	if err != nil {
		return nil, err
	}
	s := &Server{engine: l, store: store}
	s.handler = New(l).WithCleaner(s).WithReader(s)
	return s, nil
}

// Dispatch implements the transport-agnostic entry point.
func (s *Server) Dispatch(ctx context.Context, raw []byte) *Response {
	return s.handler.Dispatch(ctx, raw)
}

// Clean implements Cleaner.
func (s *Server) Clean(p map[string]any) (map[string]any, error) {
	var ids []string
	switch {
	case p["all"] == true:
		entries, err := os.ReadDir(s.store.Dir())
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			n := e.Name()
			if strings.HasSuffix(n, ".json") || strings.HasSuffix(n, ".json.corrupt") {
				id := strings.TrimSuffix(strings.TrimSuffix(n, ".json"), ".corrupt")
				if validID(id) {
					ids = append(ids, id)
				}
			}
		}
	case p["id"] != nil:
		id, _ := p["id"].(string)
		if !validID(id) {
			return nil, fmt.Errorf("invalid id")
		}
		ids = append(ids, id)
	case p["url"] != nil && p["target"] != nil:
		u, _ := p["url"].(string)
		t, _ := p["target"].(string)
		key, _ := p["task_key"].(string)
		ids = append(ids, core.IDForTask(key, u, t))
	default:
		return nil, fmt.Errorf("clean needs id, url+target, or all")
	}

	removed, missing, busy := []string{}, []string{}, []string{}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		lockCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		release, err := s.store.TaskLock(lockCtx, id)
		cancel()
		if err != nil {
			busy = append(busy, id)
			continue
		}
		st, _ := s.store.Get(id)
		_, corruptErr := os.Stat(s.store.Path(id) + ".corrupt")
		cands := []string{s.store.Path(id), s.store.Path(id) + ".corrupt"}
		if st != nil {
			part := st.PartPath
			if part == "" && st.Request.Target != "" {
				part = st.Request.Target + ".part"
			}
			if part != "" {
				cands = append(cands, part, part+".aria2")
			}
			if st.Request.Target != "" {
				cands = append(cands, st.Request.Target+".corrupt")
			}
		}
		for _, c := range cands {
			if err := os.Remove(c); err == nil {
				removed = append(removed, c)
			} else if c == s.store.Path(id) && corruptErr != nil && os.IsNotExist(err) {
				missing = append(missing, c)
			}
		}
		release()
	}
	return map[string]any{
		"ok": true, "removed": removed, "not_found": missing, "busy": busy,
		"note": "state and partial files dropped; lock files are kernel-managed",
	}, nil
}

// Read implements Reader with a bounded excerpt.
func (s *Server) Read(p map[string]any) (map[string]any, error) {
	target, _ := p["target"].(string)
	if target == "" {
		return nil, fmt.Errorf("read requires target")
	}
	opt := core.ReadOptions{MaxBytes: core.DefaultReadMax}
	if v, ok := p["grep"].(string); ok {
		opt.Pattern = v
	}
	if v, ok := p["context"].(float64); ok {
		opt.Context = int(v)
	}
	if v, ok := p["head"].(float64); ok {
		opt.Head = int(v)
	}
	if v, ok := p["tail"].(float64); ok {
		opt.Tail = int(v)
	}
	if v, ok := p["max_bytes"].(float64); ok && v > 0 {
		opt.MaxBytes = int64(v)
	}
	text, err := core.ReadFileSlice(target, opt)
	if err != nil {
		return nil, err
	}
	// read is bounded *text* access. Refuse binary bytes instead of dumping
	// replacement-character garbage into an agent's context.
	if !utf8.ValidString(text) {
		return nil, fmt.Errorf("target is not valid UTF-8 text; use grep on the file directly")
	}
	return map[string]any{"text": text, "truncated": int64(len(text)) >= opt.MaxBytes}, nil
}

func validID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
