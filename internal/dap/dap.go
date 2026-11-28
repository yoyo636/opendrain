// Package dap implements DAP v1 (Download Action Protocol) in a transport-
// agnostic way. A Dispatcher turns a Request into a Response; bindings such as
// NDJSON-over-stdio only frame the messages.
package dap

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"opendrain/internal/action"
	"opendrain/internal/core"
)

const Version = "1.0"

var Methods = []string{"capabilities", "act", "manifest", "verify", "clean", "read"}

// Request is one DAP request envelope.
type Request struct {
	DAP    string          `json:"dap"`
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// Response is one DAP response envelope. Exactly one of Result/Error is set.
type Response struct {
	DAP    string        `json:"dap"`
	ID     string        `json:"id"`
	Result any           `json:"result,omitempty"`
	Error  *core.ErrInfo `json:"error,omitempty"`
}

// ErrInfo aliases core.ErrInfo so protocol errors share the same shape as
// download errors.
type ErrInfo = core.ErrInfo

func fail(id, code, msg string, detail map[string]any) *Response {
	return &Response{DAP: Version, ID: id, Error: &core.ErrInfo{Code: code, Message: msg, Detail: detail}}
}
func ok(id string, result any) *Response {
	return &Response{DAP: Version, ID: id, Result: result}
}

// Engine is the server-side capability the dispatcher needs. It is an
// interface so conformance tests can use a real action.Layer or a fake.
type Engine interface {
	Act(context.Context, action.Intent) *action.Handle
	Manifest(bool) (*core.Manifest, error)
	Verify(string) (*core.ManifestEntry, error)
}

// Dispatcher routes requests to DAP methods. It holds no connection state.
type Dispatcher struct {
	engine   Engine
	cleaner  Cleaner
	readable Reader
}

type Cleaner interface {
	Clean(params map[string]any) (map[string]any, error)
}
type Reader interface {
	Read(params map[string]any) (map[string]any, error)
}

func New(e Engine) *Dispatcher { return &Dispatcher{engine: e} }

func (d *Dispatcher) WithCleaner(c Cleaner) *Dispatcher { d.cleaner = c; return d }
func (d *Dispatcher) WithReader(r Reader) *Dispatcher   { d.readable = r; return d }

func (d *Dispatcher) Dispatch(ctx context.Context, raw []byte) *Response {
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		return fail("", core.CodeBadInput, "invalid JSON request: "+err.Error(), nil)
	}
	if req.DAP != Version {
		return fail(req.ID, "unsupported_version",
			fmt.Sprintf("unsupported DAP version %q, want %q", req.DAP, Version),
			map[string]any{"supported": []string{Version}})
	}
	if req.ID == "" {
		return fail("", core.CodeBadInput, "id is required", nil)
	}
	if req.Params == nil {
		req.Params = json.RawMessage(`{}`)
	}
	if !isMethod(req.Method) {
		return fail(req.ID, "unsupported_method", "unsupported method "+req.Method,
			map[string]any{"methods": Methods})
	}

	switch req.Method {
	case "capabilities":
		return ok(req.ID, map[string]any{
			"dap": Version, "methods": Methods,
			"backends": []string{"native", "aria2"},
			"features": []string{"resume", "mirrors", "checksum", "task_key", "integrity_flag"},
		})
	case "act":
		var in action.Intent
		if err := json.Unmarshal(req.Params, &in); err != nil {
			return fail(req.ID, core.CodeBadInput, "invalid act params: "+err.Error(), nil)
		}
		if in.URL == "" || in.Target == "" {
			return fail(req.ID, core.CodeBadInput, "act requires url and target", nil)
		}
		if in.Checksum != "" {
			algo, _, err := core.ParseChecksum(in.Checksum)
			if err != nil || algo != "sha256" {
				return fail(req.ID, core.CodeBadInput, err.Error(), nil)
			}
		}
		h := d.engine.Act(ctx, in)
		if h == nil || h.Error != nil {
			resp := fail(req.ID, codeOf(h), messageOf(h), nil)
			if h != nil && h.Error != nil {
				resp.Error = h.Error
			}
			return resp
		}
		return ok(req.ID, h)
	case "manifest":
		var p struct {
			Verify bool `json:"verify"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return fail(req.ID, core.CodeBadInput, "invalid manifest params: "+err.Error(), nil)
		}
		m, err := d.engine.Manifest(p.Verify)
		if err != nil {
			return fail(req.ID, core.CodeInternal, err.Error(), nil)
		}
		return ok(req.ID, m)
	case "verify":
		var p struct {
			Target string `json:"target"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil || p.Target == "" {
			return fail(req.ID, core.CodeBadInput, "verify requires target", nil)
		}
		ent, err := d.engine.Verify(p.Target)
		if err != nil {
			return fail(req.ID, core.CodeInternal, err.Error(), nil)
		}
		if ent == nil {
			return fail(req.ID, core.CodeNotFound, "no managed task for target "+p.Target, nil)
		}
		return ok(req.ID, ent)
	case "clean":
		if d.cleaner == nil {
			return fail(req.ID, "unsupported_method", "clean not supported by this server", nil)
		}
		var p map[string]any
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return fail(req.ID, core.CodeBadInput, "invalid clean params", nil)
		}
		result, err := d.cleaner.Clean(p)
		if err != nil {
			return fail(req.ID, core.CodeInternal, err.Error(), nil)
		}
		return ok(req.ID, result)
	case "read":
		if d.readable == nil {
			return fail(req.ID, "unsupported_method", "read not supported by this server", nil)
		}
		var p map[string]any
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return fail(req.ID, core.CodeBadInput, "invalid read params", nil)
		}
		result, err := d.readable.Read(p)
		if err != nil {
			return fail(req.ID, core.CodeInternal, err.Error(), nil)
		}
		return ok(req.ID, result)
	}
	return fail(req.ID, core.CodeInternal, "unreachable", nil)
}

func isMethod(m string) bool {
	for _, known := range Methods {
		if m == known {
			return true
		}
	}
	return false
}

func codeOf(h *action.Handle) string {
	if h == nil || h.Error == nil {
		return core.CodeInternal
	}
	return h.Error.Code
}
func messageOf(h *action.Handle) string {
	if h == nil {
		return "empty handle"
	}
	if h.Error != nil {
		return h.Error.Message
	}
	return "download failed"
}

// Encode is the canonical JSON encoding of a response (one line).
func Encode(resp *Response) ([]byte, error) {
	b, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// Decode parses one canonical response line.
func Decode(line []byte) (*Response, error) {
	var resp Response
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(line))), &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
