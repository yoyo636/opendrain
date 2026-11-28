package action

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"opendrain/internal/core"
)

// stubDownloader writes a fixed payload into the part file, so the DAL's
// policy can be tested without touching the network or a real backend.
type stubDownloader struct {
	payload []byte
	name    string
}

func (s *stubDownloader) Name() string {
	if s.name == "" {
		return "native"
	}
	return s.name
}
func (s *stubDownloader) Available(context.Context) bool { return true }
func (s *stubDownloader) Status(string) (*core.State, error) {
	return nil, nil
}
func (s *stubDownloader) Cancel(string) error { return nil }
func (s *stubDownloader) Start(_ context.Context, st *core.State, _ *core.Store) error {
	return os.WriteFile(st.PartPath, s.payload, 0o644)
}

// probeServer answers the Range: bytes=0-0 probe; the body itself comes from
// the stub backend.
func probeServer(t *testing.T, size int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-0/"+itoa(size))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("x"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The DAL must own policy: a zero-value Intent still means "resume, auto
// backend". Agents should not have to know backend names or chunking rules.
func TestNormalizeDefaults(t *testing.T) {
	got := Intent{URL: "http://x", Target: "/tmp/x"}.Normalize()
	if !got.Resume || got.Backend != "auto" {
		t.Errorf("normalize = %+v, want resume=true backend=auto", got)
	}
}

func TestActReturnsHandleNotContent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENDRAIN_STATE_DIR", filepath.Join(dir, "state"))
	payload := []byte(strings.Repeat("payload-", 1024))
	sum := sha256.Sum256(payload)
	checksum := "sha256:" + hex.EncodeToString(sum[:])
	srv := probeServer(t, len(payload))

	l, err := New()
	if err != nil {
		t.Fatal(err)
	}
	l.backends = []core.Downloader{&stubDownloader{payload: payload}}

	target := filepath.Join(dir, "f.bin")
	h := l.Act(context.Background(), Intent{URL: srv.URL, Target: target, Checksum: checksum})
	if !h.OK() {
		t.Fatalf("Act failed: %+v", h.Error)
	}
	if h.Bytes != int64(len(payload)) || h.Sha256 != checksum || h.Integrity != "checksum" {
		t.Errorf("bad handle: %+v", h)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("target missing: %v", err)
	}
}

func TestActIsIdempotentAcrossSessions(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENDRAIN_STATE_DIR", filepath.Join(dir, "state"))
	payload := []byte("stable-bytes-1234")
	srv := probeServer(t, len(payload))

	l, err := New()
	if err != nil {
		t.Fatal(err)
	}
	l.backends = []core.Downloader{&stubDownloader{payload: payload}}
	target := filepath.Join(dir, "f.bin")
	intent := Intent{URL: srv.URL, Target: target}

	first := l.Act(context.Background(), intent)
	if !first.OK() {
		t.Fatalf("first Act failed: %+v", first.Error)
	}
	// A fresh Layer instance simulates a new agent session.
	l2, _ := New()
	l2.backends = []core.Downloader{&stubDownloader{payload: []byte("should-not-be-used")}}
	second := l2.Act(context.Background(), intent)
	if !second.OK() || !second.Cached {
		t.Fatalf("second Act should hit the DAL's memory: %+v", second)
	}
	if first.ID != second.ID || second.Sha256 != first.Sha256 {
		t.Errorf("handles disagree across sessions: %+v vs %+v", first, second)
	}

	m, err := l2.Manifest(false)
	if err != nil || m.Count != 1 || m.OnDisk != 1 {
		t.Errorf("manifest wrong: %+v %v", m, err)
	}
	ent, err := l2.Verify(target)
	if err != nil || ent == nil || ent.Verified == nil || !*ent.Verified {
		t.Errorf("verify wrong: %+v %v", ent, err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
