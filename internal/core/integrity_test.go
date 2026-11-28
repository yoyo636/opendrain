package core

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// probeServer answers the Range: bytes=0-0 probe with 206 and the size the
// test wants the caller to believe. Fake backends do the "download", so the
// server never needs to serve a full body.
func probeServer(t *testing.T, size int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", size))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte("x"))
			return
		}
		w.Header().Set("Content-Length", fmt.Sprintf("%d", size))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("x"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeBackend lets a core test control what Start sees and writes without
// pulling in the real native/aria2 implementations.
type fakeBackend struct {
	name    string
	payload []byte
	err     error
	sawPart bool
}

func (f *fakeBackend) Name() string                   { return f.name }
func (f *fakeBackend) Available(context.Context) bool { return true }
func (f *fakeBackend) Status(string) (*State, error)  { return nil, nil }
func (f *fakeBackend) Cancel(string) error            { return nil }
func (f *fakeBackend) Start(_ context.Context, st *State, _ *Store) error {
	if _, err := os.Stat(st.PartPath); err == nil {
		f.sawPart = true
	}
	if f.err != nil {
		return f.err
	}
	return os.WriteFile(st.PartPath, f.payload, 0o644)
}

func TestFreshTaskDiscardsOrphanPart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENDRAIN_STATE_DIR", filepath.Join(dir, "state"))
	store, err := NewStore()
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(strings.Repeat("good-bytes-", 1<<10)) // 11KB
	target := filepath.Join(dir, "f.bin")
	orphan := target + ".part"
	if err := os.WriteFile(orphan, []byte("STALE"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := probeServer(t, int64(len(payload)))
	be := &fakeBackend{name: "native", payload: payload}

	res, _ := Fetch(context.Background(), Request{
		URL: srv.URL, Target: target, Resume: true, Backend: "native",
	}, store, []Downloader{be})
	if !res.OK {
		t.Fatalf("fetch failed: %+v", res.Error)
	}
	if be.sawPart {
		t.Error("fresh task must not let a backend see an orphan .part file")
	}
	if !strings.Contains(strings.Join(res.Warnings, " "), "stale partial") {
		t.Errorf("discard should be reported in warnings, got %v", res.Warnings)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("published bytes wrong: %q %v", got, err)
	}
}

func TestResumedTaskKeepsItsOwnPart(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENDRAIN_STATE_DIR", filepath.Join(dir, "state"))
	store, err := NewStore()
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(strings.Repeat("resume-", 1<<10))
	target := filepath.Join(dir, "f.bin")
	srv := probeServer(t, int64(len(payload)))
	id := IDFor(srv.URL, target)
	if err := os.WriteFile(target+".part", []byte("KEEP"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := &State{ID: id, Status: StatusCancelled,
		Request:  Request{URL: srv.URL, Target: target, Resume: true, Backend: "native"},
		PartPath: target + ".part", Mode: ModeSingle, Total: int64(len(payload)), Done: 4,
		Chunks: []Chunk{{Start: 0, End: int64(len(payload)) - 1, Done: 4}},
	}
	if err := store.Put(st); err != nil {
		t.Fatal(err)
	}
	be := &fakeBackend{name: "native", payload: payload}

	// The probe server answers Range, so progress is considered reusable and
	// the backend must still see the existing .part.
	res, _ := Fetch(context.Background(), Request{
		URL: srv.URL, Target: target, Resume: true, Backend: "native",
	}, store, []Downloader{be})
	if !res.OK {
		t.Fatalf("resume fetch failed: %+v", res.Error)
	}
	if !res.Resumed {
		t.Error("expected resumed=true for a reusable part")
	}
	if !be.sawPart {
		t.Error("a resumable task with an existing part must keep it")
	}
}

func TestUnknownBackendRejected(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENDRAIN_STATE_DIR", filepath.Join(dir, "state"))
	store, _ := NewStore()
	srv := probeServer(t, 10)
	be := &fakeBackend{name: "native", payload: []byte("0123456789")}
	res, _ := Fetch(context.Background(), Request{
		URL: srv.URL, Target: filepath.Join(dir, "f.bin"), Resume: true, Backend: "bogus",
	}, store, []Downloader{be})
	if res.Error == nil || res.Error.Code != CodeBadInput ||
		!strings.Contains(res.Error.Message, "unknown backend") {
		t.Fatalf("unknown backend must be bad_request, got %+v", res.Error)
	}
}

func TestIDForTaskUsesCallerKey(t *testing.T) {
	a := IDForTask("hf:model@rev1", "https://cas-bridge.xethub.hf.co/x?sig=old&Expires=1", "/tmp/model.bin")
	b := IDForTask("hf:model@rev1", "https://cas-bridge.xethub.hf.co/x?sig=new&Expires=2", "/tmp/model.bin")
	if a != b {
		t.Error("same task key must keep one identity across URL refreshes")
	}
	if a == IDForTask("hf:model@rev2", "https://cas-bridge.xethub.hf.co/x?sig=old", "/tmp/model.bin") {
		t.Error("different task key must produce a different id")
	}
	if a == IDForTask("hf:model@rev1", "https://cas-bridge.xethub.hf.co/x", "/tmp/other.bin") {
		t.Error("different target must produce a different id")
	}
	if IDForTask("  ", "http://x/y", "/tmp/f") != IDFor("http://x/y", "/tmp/f") {
		t.Error("blank task key must fall back to url+target")
	}
}

func TestStateFilesArePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not meaningful on Windows")
	}
	dir := filepath.Join(t.TempDir(), "state")
	t.Setenv("OPENDRAIN_STATE_DIR", dir)
	store, err := NewStore()
	if err != nil {
		t.Fatal(err)
	}
	id := IDFor("http://x/y", "/tmp/z")
	if err := store.Put(&State{ID: id, Status: StatusDownloading, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Errorf("state dir mode = %o, want 700", di.Mode().Perm())
	}
	fi, err := os.Stat(store.Path(id))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("state file mode = %o, want 600", fi.Mode().Perm())
	}
}

func TestVerifyTargetHashesOnlyThatFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPENDRAIN_STATE_DIR", filepath.Join(dir, "state"))
	store, _ := NewStore()
	target := filepath.Join(dir, "dataset.bin")
	body := []byte("trusted bytes")
	if err := os.WriteFile(target, body, 0o644); err != nil {
		t.Fatal(err)
	}
	sum, _ := FileSha256(target)
	id := IDFor("http://x/dataset", target)
	if err := store.Put(&State{ID: id, Status: StatusCompleted,
		Request:   Request{URL: "http://x/dataset", Target: target},
		Total:     int64(len(body)),
		Sha256:    "sha256:" + sum,
		CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	ent, err := store.VerifyTarget(target)
	if err != nil || ent == nil {
		t.Fatalf("VerifyTarget: %v %v", ent, err)
	}
	if !ent.OnDisk || ent.Verified == nil || !*ent.Verified {
		t.Errorf("expected verified entry, got %+v", ent)
	}
	if none, err := store.VerifyTarget(filepath.Join(dir, "unmanaged")); err != nil || none != nil {
		t.Errorf("unmanaged target should be nil,nil: %v %v", none, err)
	}

	if err := os.WriteFile(target, []byte("tampered!!!!!"), 0o644); err != nil {
		t.Fatal(err)
	}
	ent, _ = store.VerifyTarget(target)
	if ent == nil || ent.Verified == nil || *ent.Verified {
		t.Errorf("tampered file must fail verification, got %+v", ent)
	}
	if !strings.Contains(ent.Note, "mismatch") {
		t.Errorf("missing mismatch note: %+v", ent)
	}
}
