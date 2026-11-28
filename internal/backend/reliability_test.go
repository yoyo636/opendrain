package backend

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"opendrain/internal/core"
)

// A server that ignores Range cannot be resumed from an offset. Interrupting
// one used to leave a state that failed forever (the 200-restart branch was
// unreachable because `acceptable` rejected 200 when done > 0). The only
// honest behaviour is to restart from zero and report resumed=false.
func TestNonRangeResumeRestartsFromZero(t *testing.T) {
	data := randData(8 << 20)
	srv := newRangeServer(t, data, srvOpts{ignoreRange: true, slowFor: 800 * time.Millisecond})
	backends := testBackends(t)
	target := filepath.Join(t.TempDir(), "f.bin")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(400 * time.Millisecond)
		cancel()
	}()
	res1, _ := core.Fetch(ctx, core.Request{URL: srv.srv.URL, Target: target, Resume: true, Backend: "native"},
		mustStore(t), backends)
	if res1.Status != core.StatusCancelled {
		t.Fatalf("expected cancelled, got %s (%+v)", res1.Status, res1.Error)
	}
	if fi, err := os.Stat(target + ".part"); err != nil || fi.Size() == 0 {
		t.Fatalf("cancellation should keep a non-empty part: %v", err)
	}

	time.Sleep(900 * time.Millisecond) // let the throttle window expire
	res2, st2 := core.Fetch(context.Background(),
		core.Request{URL: srv.srv.URL, Target: target, Resume: true, Backend: "native"},
		mustStore(t), backends)
	if !res2.OK {
		t.Fatalf("non-Range re-fetch failed: %+v", res2.Error)
	}
	if res2.Resumed {
		t.Error("a non-Range download cannot resume; resumed must be false")
	}
	if st2.Mode != core.ModeSingle {
		t.Errorf("expected single mode, got %s", st2.Mode)
	}
	if res2.Sha256 != digest(data) || res2.Bytes != int64(len(data)) {
		t.Errorf("restarted download is wrong: %s %d", res2.Sha256, res2.Bytes)
	}
}

// A completed target that was truncated must not be reported as a cache hit
// with a fresh digest: quarantine the evidence and download the real bytes.
func TestTruncatedCachedFileSelfHeals(t *testing.T) {
	data := randData(10 << 20)
	srv := newRangeServer(t, data, srvOpts{})
	backends := testBackends(t)
	target := filepath.Join(t.TempDir(), "f.bin")
	req := core.Request{URL: srv.srv.URL, Target: target, Resume: true, Backend: "native"}

	if res, _ := core.Fetch(context.Background(), req, mustStore(t), backends); !res.OK {
		t.Fatalf("first fetch failed: %+v", res.Error)
	}
	if err := os.Truncate(target, 1<<20); err != nil {
		t.Fatal(err)
	}

	res, st := core.Fetch(context.Background(), req, mustStore(t), backends)
	if !res.OK {
		t.Fatalf("self-healing fetch failed: %+v", res.Error)
	}
	if res.Cached {
		t.Error("a size-mismatched file must not be served from cache")
	}
	if res.Resumed {
		t.Error("re-download after quarantine must not claim resume")
	}
	if res.Bytes != int64(len(data)) || res.Sha256 != digest(data) {
		t.Errorf("wrong bytes after heal: %d %s", res.Bytes, res.Sha256)
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("target bytes not restored: %v", err)
	}
	if fi, err := os.Stat(target + ".corrupt"); err != nil || fi.Size() != 1<<20 {
		t.Errorf("truncated evidence should be kept as .corrupt: %v", err)
	}
	if !strings.Contains(strings.Join(res.Warnings, " "), "does not match recorded") {
		t.Errorf("heal should be explained in warnings, got %v", res.Warnings)
	}
	_ = st
}

// auto must not run an aria2 fallback that is not installed: doing so replaced
// the real native failure with "cannot start aria2c: exec: no command".
type failingDownloader struct{}

func (failingDownloader) Name() string                   { return "native" }
func (failingDownloader) Available(context.Context) bool { return true }
func (failingDownloader) Start(context.Context, *core.State, *core.Store) error {
	return core.Fail(core.CodeAllFailed, "primary native failure", nil)
}
func (failingDownloader) Status(string) (*core.State, error) { return nil, nil }
func (failingDownloader) Cancel(string) error                { return nil }

func TestAutoFallbackSkipsUnavailableAria2(t *testing.T) {
	data := randData(1 << 20)
	srv := newRangeServer(t, data, srvOpts{})
	testBackends(t) // sets OPENDRAIN_STATE_DIR
	target := filepath.Join(t.TempDir(), "f.bin")
	res, _ := core.Fetch(context.Background(),
		core.Request{URL: srv.srv.URL, Target: target, Resume: true, Backend: "auto"},
		mustStore(t), []core.Downloader{&Aria2{Bin: ""}, failingDownloader{}})
	if res.Error == nil {
		t.Fatalf("expected failure, got %+v", res)
	}
	if res.Error.Code != core.CodeAllFailed || !strings.Contains(res.Error.Message, "primary native failure") {
		t.Errorf("primary failure must survive, got %+v", res.Error)
	}
	if strings.Contains(res.Error.Message, "aria2c") {
		t.Errorf("unavailable fallback must not mask the error: %+v", res.Error)
	}
	if res.Backend != "native" {
		t.Errorf("backend should stay native, got %s", res.Backend)
	}
}

// Real aria2 integration (the fake RPC test cannot exercise --continue's
// semantics). This is the P0 regression: a fresh task with an orphan part
// must not let aria2 treat those bytes as a downloaded prefix.
func TestAria2RealDoesNotReuseOrphanPart(t *testing.T) {
	if _, err := exec.LookPath("aria2c"); err != nil {
		t.Skip("aria2c not installed")
	}
	data := randData(2 << 20)
	cases := []struct {
		name string
		size int
		fill byte
	}{
		{"partial-prefix", 1 << 20, 'X'},
		{"full-size-zeros", 2 << 20, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newRangeServer(t, data, srvOpts{})
			backends := testBackends(t)
			target := filepath.Join(t.TempDir(), "f.bin")
			part := make([]byte, tc.size)
			if tc.fill != 0 {
				for i := range part {
					part[i] = tc.fill
				}
			}
			if err := os.WriteFile(target+".part", part, 0o644); err != nil {
				t.Fatal(err)
			}

			res, _ := core.Fetch(context.Background(),
				core.Request{URL: srv.srv.URL, Target: target, Resume: true, Backend: "aria2"},
				mustStore(t), backends)
			if !res.OK {
				t.Fatalf("aria2 fetch failed: %+v", res.Error)
			}
			got, err := os.ReadFile(target)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("aria2 published orphan bytes (got %d bytes, equal=%v)", len(got), bytes.Equal(got, data))
			}
		})
	}
}

// A connection that trickles a few bytes keeps the inactivity timer alive
// forever. The rate window must abort it instead of hanging the whole fetch.
type trickleReader struct{}

func (trickleReader) Read(p []byte) (int, error) {
	time.Sleep(5 * time.Millisecond)
	p[0] = 0
	return 1, nil
}
func (trickleReader) Close() error { return nil }

func TestStallBodyAbortsTrickle(t *testing.T) {
	oldStall, oldWindow, oldRate := stallTimeout, rateWindow, minReadRate
	stallTimeout, rateWindow, minReadRate = 2*time.Second, 50*time.Millisecond, 1024
	t.Cleanup(func() { stallTimeout, rateWindow, minReadRate = oldStall, oldWindow, oldRate })

	sb := newStallBody(trickleReader{}, func() {})
	defer sb.Close()
	buf := make([]byte, 16)
	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("trickling body was not aborted")
		default:
		}
		if _, err := sb.Read(buf); err != nil {
			if !strings.Contains(err.Error(), "stalled") {
				t.Fatalf("unexpected abort error: %v", err)
			}
			return
		}
	}
}
