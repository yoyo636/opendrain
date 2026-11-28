package dap

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"opendrain/internal/action"
	"opendrain/internal/core"
)

// conformanceEngine is a minimal in-memory Engine for protocol conformance.
type conformanceEngine struct{ calls int }

func (e *conformanceEngine) Act(_ context.Context, in action.Intent) *action.Handle {
	e.calls++
	return &action.Handle{
		ID: "id-1", Target: "/tmp/f", Bytes: 4, Sha256: "sha256:" + strings.Repeat("a", 64),
		Integrity: "unverified", Status: core.StatusCompleted,
	}
}
func (e *conformanceEngine) Manifest(bool) (*core.Manifest, error) {
	return &core.Manifest{OK: true, Entries: []core.ManifestEntry{}}, nil
}
func (e *conformanceEngine) Verify(t string) (*core.ManifestEntry, error) {
	if strings.Contains(t, "missing") {
		return nil, nil
	}
	return &core.ManifestEntry{ID: "id-1", Target: t, OnDisk: true}, nil
}

func roundTrip(t *testing.T, d *Dispatcher, req map[string]any) *Response {
	t.Helper()
	b, _ := json.Marshal(req)
	resp := d.Dispatch(context.Background(), b)
	if resp == nil || resp.DAP != Version {
		t.Fatalf("bad response envelope: %+v", resp)
	}
	if resp.ID != req["id"] {
		t.Errorf("correlation id lost: %+v", resp)
	}
	if (resp.Result == nil) == (resp.Error == nil) {
		t.Errorf("exactly one of result/error must be set: %+v", resp)
	}
	return resp
}

func TestConformanceCapabilities(t *testing.T) {
	d := New(&conformanceEngine{})
	resp := roundTrip(t, d, map[string]any{"dap": "1.0", "id": "c1", "method": "capabilities", "params": map[string]any{}})
	if resp.Error != nil {
		t.Fatalf("capabilities failed: %+v", resp.Error)
	}
	result, _ := resp.Result.(map[string]any)
	methods, _ := result["methods"].([]string)
	if len(methods) == 0 || result["dap"] != Version {
		t.Errorf("bad capabilities: %+v", result)
	}
}

func TestConformanceActRejectsBadIntent(t *testing.T) {
	d := New(&conformanceEngine{})
	bad := []map[string]any{
		{"dap": "1.0", "id": "b1", "method": "act", "params": map[string]any{"target": "/tmp/f"}},
		{"dap": "1.0", "id": "b2", "method": "act", "params": map[string]any{"url": "http://x"}},
		{"dap": "1.0", "id": "b3", "method": "act", "params": map[string]any{"url": "http://x", "target": "/tmp/f", "checksum": "md5:abc"}},
	}
	for _, req := range bad {
		resp := roundTrip(t, d, req)
		if resp.Error == nil || resp.Error.Code != core.CodeBadInput {
			t.Errorf("%v: expected bad_request, got %+v", req["id"], resp)
		}
	}
}

func TestConformanceActSucceeds(t *testing.T) {
	e := &conformanceEngine{}
	d := New(e)
	resp := roundTrip(t, d, map[string]any{
		"dap": "1.0", "id": "a1", "method": "act",
		"params": map[string]any{"url": "http://x/f", "target": "/tmp/f"},
	})
	if resp.Error != nil {
		t.Fatalf("act failed: %+v", resp.Error)
	}
	h, ok := resp.Result.(*action.Handle)
	if !ok || h.Status != core.StatusCompleted {
		t.Fatalf("bad handle: %#v", resp.Result)
	}
	if h.Bytes != 4 || h.Integrity != "unverified" {
		t.Errorf("handle semantics wrong: %+v", h)
	}
	if e.calls != 1 {
		t.Errorf("act called %d times, want 1", e.calls)
	}
	// The handle must serialize without content.
	b, _ := json.Marshal(h)
	if strings.Contains(string(b), "content") {
		t.Errorf("handle leaked content: %s", b)
	}
}

func TestConformanceVersionMethodAndErrors(t *testing.T) {
	d := New(&conformanceEngine{})
	cases := []struct {
		req  map[string]any
		code string
	}{
		{map[string]any{"dap": "2.0", "id": "v1", "method": "act", "params": map[string]any{}}, "unsupported_version"},
		{map[string]any{"dap": "1.0", "id": "m1", "method": "bogus", "params": map[string]any{}}, "unsupported_method"},
		{map[string]any{"dap": "1.0", "method": "act", "params": map[string]any{}}, core.CodeBadInput},
	}
	for _, c := range cases {
		resp := d.Dispatch(context.Background(), mustJSON(c.req))
		if resp.Error == nil || resp.Error.Code != c.code {
			t.Errorf("%v: got %+v, want %s", c.req, resp, c.code)
		}
	}
	// Malformed JSON.
	resp := d.Dispatch(context.Background(), []byte(`{`))
	if resp.Error == nil || resp.Error.Code != core.CodeBadInput {
		t.Errorf("malformed JSON: %+v", resp)
	}
}

func TestConformanceVerifyAndManifest(t *testing.T) {
	d := New(&conformanceEngine{})
	resp := roundTrip(t, d, map[string]any{
		"dap": "1.0", "id": "v1", "method": "verify", "params": map[string]any{"target": "/tmp/f"},
	})
	if resp.Error != nil {
		t.Fatalf("verify failed: %+v", resp.Error)
	}
	resp = roundTrip(t, d, map[string]any{
		"dap": "1.0", "id": "n1", "method": "verify", "params": map[string]any{"target": "/tmp/missing"},
	})
	if resp.Error == nil || resp.Error.Code != core.CodeNotFound {
		t.Errorf("missing target: %+v", resp)
	}
	resp = roundTrip(t, d, map[string]any{
		"dap": "1.0", "id": "mf1", "method": "manifest", "params": map[string]any{"verify": true},
	})
	if resp.Error != nil {
		t.Fatalf("manifest failed: %+v", resp.Error)
	}
}

func TestConformanceEncodeDecode(t *testing.T) {
	d := New(&conformanceEngine{})
	resp := d.Dispatch(context.Background(), mustJSON(map[string]any{
		"dap": "1.0", "id": "e1", "method": "capabilities", "params": map[string]any{},
	}))
	b, err := Encode(resp)
	if err != nil || !strings.HasSuffix(string(b), "\n") {
		t.Fatalf("encode: %q %v", b, err)
	}
	decoded, err := Decode(b)
	if err != nil || decoded.ID != "e1" || decoded.DAP != Version {
		t.Fatalf("decode: %+v %v", decoded, err)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func TestConformanceClientOverPipe(t *testing.T) {
	// Wire the real Dispatcher to an in-memory pipe via a tiny line server.
	// client writes -> serverIn ; server writes -> clientIn
	serverIn, clientWriter := io.Pipe()
	clientIn, serverOut := io.Pipe()
	defer serverIn.Close()
	defer clientWriter.Close()
	defer clientIn.Close()
	defer serverOut.Close()

	srv := New(&conformanceEngine{})
	go func() {
		sc := bufio.NewScanner(serverIn)
		for sc.Scan() {
			resp := srv.Dispatch(context.Background(), sc.Bytes())
			b, _ := Encode(resp)
			serverOut.Write(b)
		}
	}()

	client := NewClient(clientWriter, clientIn)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h, err := client.Download(ctx, action.Intent{URL: "http://x/f", Target: "/tmp/f"})
	if err != nil {
		t.Fatalf("client download: %v", err)
	}
	if h == nil || h.Status != core.StatusCompleted || h.Bytes != 4 {
		t.Fatalf("bad handle from client: %+v", h)
	}
}
