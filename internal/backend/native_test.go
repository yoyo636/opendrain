package backend

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"opendrain/internal/core"
)

func testBackends(tb testing.TB) []core.Downloader {
	tb.Setenv("OPENDRAIN_STATE_DIR", filepath.Join(tb.TempDir(), "state"))
	return []core.Downloader{NewAria2(), NewNative()}
}

func digest(b []byte) string {
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

func randData(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func TestFetchChunkedCompletesWithSha(t *testing.T) {
	data := randData(10 << 20)
	srv := newRangeServer(t, data, srvOpts{})
	backends := testBackends(t)
	target := filepath.Join(t.TempDir(), "f.bin")

	res, st := core.Fetch(context.Background(), core.Request{
		URL: srv.srv.URL, Target: target, Resume: true, Backend: "native",
	}, mustStore(t), backends)
	if !res.OK {
		t.Fatalf("fetch failed: %+v", res.Error)
	}
	if res.Backend != "native" {
		t.Errorf("expected native backend (no aria2 injected), got %s", res.Backend)
	}
	if res.Sha256 != digest(data) {
		t.Errorf("sha mismatch: %s", res.Sha256)
	}
	if st.Mode != core.ModeRanged {
		t.Errorf("expected ranged mode, got %s", st.Mode)
	}
	if len(st.Chunks) < 2 {
		t.Errorf("expected >=2 chunks for 10MB, got %d", len(st.Chunks))
	}
	got, err := os.ReadFile(target)
	if err != nil || len(got) != len(data) {
		t.Fatalf("target file wrong: len=%d err=%v", len(got), err)
	}
	if _, err := os.Stat(target + ".part"); !os.IsNotExist(err) {
		t.Errorf("part file should be gone after publish")
	}
}

func TestFetchResumeAfterCancellation(t *testing.T) {
	data := randData(12 << 20)
	// Throttle the first 1.2s so cancellation lands mid-download.
	srv := newRangeServer(t, data, srvOpts{slowFor: 1200 * time.Millisecond})
	backends := testBackends(t)
	target := filepath.Join(t.TempDir(), "f.bin")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(400 * time.Millisecond)
		cancel()
	}()
	res1, _ := core.Fetch(ctx, core.Request{URL: srv.srv.URL, Target: target, Resume: true},
		mustStore(t), backends)
	if res1.Status != core.StatusCancelled {
		t.Fatalf("expected cancelled, got %s (%+v)", res1.Status, res1.Error)
	}

	time.Sleep(1 * time.Second) // let the throttle window expire
	res2, st2 := core.Fetch(context.Background(), core.Request{
		URL: srv.srv.URL, Target: target, Resume: true, Backend: "native",
	}, mustStore(t), backends)
	if !res2.OK {
		t.Fatalf("resume fetch failed: %+v", res2.Error)
	}
	if !res2.Resumed {
		t.Errorf("expected resumed=true")
	}
	if st2.Mode != core.ModeRanged {
		t.Errorf("expected ranged mode after resume")
	}
	if res2.Sha256 != digest(data) {
		t.Errorf("sha mismatch after resume: %s", res2.Sha256)
	}
}

func TestFetchMirrorFailoverKeepsProgress(t *testing.T) {
	data := randData(12 << 20)
	// Primary (listed first) dies mid-body on every connection; the mirror
	// serves the rest. The mirror answers the probe 60ms slower so the
	// primary is deterministically ranked first: mirror ranking is
	// latency-based and two localhost probes can easily measure the same
	// millisecond, which made this test flaky on a loaded machine.
	bad := newRangeServer(t, data, srvOpts{abortAfterBytes: 1 << 20})
	good := newRangeServer(t, data, srvOpts{probeDelay: 60 * time.Millisecond})
	backends := testBackends(t)
	target := filepath.Join(t.TempDir(), "f.bin")

	// Force the native backend on purpose: aria2 does its own mirror
	// handling, so this case has to exercise OUR failover code. On a host
	// with aria2c installed the "auto" route silently took over this test
	// and the native failover path was never covered.
	res, st := core.Fetch(context.Background(), core.Request{
		URL: bad.srv.URL, Mirrors: []string{good.srv.URL}, Target: target,
		Resume: true, Backend: "native",
	}, mustStore(t), backends)
	if !res.OK {
		t.Fatalf("failover fetch failed: %+v", res.Error)
	}
	if res.Sha256 != digest(data) {
		t.Errorf("sha mismatch: %s", res.Sha256)
	}
	if st.Selected != bad.srv.URL {
		t.Errorf("expected primary probed best first, selected=%s", st.Selected)
	}
}

func TestFetchChecksumMismatchLeavesNothing(t *testing.T) {
	data := randData(2 << 20)
	srv := newRangeServer(t, data, srvOpts{})
	backends := testBackends(t)
	target := filepath.Join(t.TempDir(), "f.bin")

	res, st := core.Fetch(context.Background(), core.Request{
		URL: srv.srv.URL, Target: target, Resume: true, Backend: "native",
		Checksum: "sha256:" + "ab",
	}, mustStore(t), backends)
	// invalid checksum format is rejected up front
	if res.Error == nil || res.Error.Code != core.CodeBadInput {
		t.Fatalf("expected bad_request for malformed checksum, got %+v", res.Error)
	}

	res, st = core.Fetch(context.Background(), core.Request{
		URL: srv.srv.URL, Target: target, Resume: true, Backend: "native",
		Checksum: digest(randData(32)),
	}, mustStore(t), backends)
	if res.Error == nil || res.Error.Code != core.CodeChecksum {
		t.Fatalf("expected checksum_mismatch, got %+v", res.Error)
	}
	if st.Status != core.StatusFailed {
		t.Errorf("state should be failed, got %s", st.Status)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target must not exist after checksum failure")
	}
	if _, err := os.Stat(target + ".part"); !os.IsNotExist(err) {
		t.Errorf("part file must be removed after checksum failure")
	}
}

func TestFetchNoRangeSingleStream(t *testing.T) {
	data := randData(2 << 20)
	srv := newRangeServer(t, data, srvOpts{ignoreRange: true})
	backends := testBackends(t)
	target := filepath.Join(t.TempDir(), "f.bin")

	res, st := core.Fetch(context.Background(), core.Request{
		URL: srv.srv.URL, Target: target, Resume: true, Backend: "native",
	}, mustStore(t), backends)
	if !res.OK {
		t.Fatalf("fetch failed: %+v", res.Error)
	}
	if st.Mode != core.ModeSingle {
		t.Errorf("expected single mode, got %s", st.Mode)
	}
	if res.Sha256 != digest(data) {
		t.Errorf("sha mismatch: %s", res.Sha256)
	}
}

func TestFetchUnknownSizeChunkedBody(t *testing.T) {
	data := randData(3 << 20)
	srv := newRangeServer(t, data, srvOpts{noLength: true})
	backends := testBackends(t)
	target := filepath.Join(t.TempDir(), "f.bin")

	res, _ := core.Fetch(context.Background(), core.Request{
		URL: srv.srv.URL, Target: target, Resume: true, Backend: "native",
	}, mustStore(t), backends)
	if !res.OK {
		t.Fatalf("fetch failed: %+v", res.Error)
	}
	if res.Bytes != int64(len(data)) {
		t.Errorf("bytes=%d, want %d", res.Bytes, len(data))
	}
	if res.Sha256 != digest(data) {
		t.Errorf("sha mismatch: %s", res.Sha256)
	}
}

func TestResumedNotReportedWhenPartFileIsGone(t *testing.T) {
	data := randData(2 << 20)
	srv := newRangeServer(t, data, srvOpts{})
	backends := testBackends(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "f.bin")
	store := mustStore(t)

	// Plant a state that claims progress while no part file exists on disk:
	// the recorded progress cannot be used, so the download restarts from
	// zero and must not claim "resumed".
	id := core.IDFor(srv.srv.URL, target)
	st := &core.State{
		ID: id, Status: core.StatusCancelled, Backend: "native",
		Request:   core.Request{URL: srv.srv.URL, Target: target, Resume: true, Backend: "native"},
		PartPath:  target + ".part",
		Mode:      core.ModeRanged,
		Total:     int64(len(data)),
		Done:      1024,
		Chunks:    []core.Chunk{{Start: 0, End: 1023, Done: 1024}},
		CreatedAt: time.Now(),
	}
	if err := store.Put(st); err != nil {
		t.Fatal(err)
	}

	res, _ := core.Fetch(context.Background(), core.Request{
		URL: srv.srv.URL, Target: target, Resume: true, Backend: "native",
	}, store, backends)
	if !res.OK {
		t.Fatalf("fetch failed: %+v", res.Error)
	}
	if res.Resumed {
		t.Errorf("resumed must be false: the part file was gone, so nothing was resumed")
	}
	if res.Sha256 != digest(data) {
		t.Errorf("sha mismatch: %s", res.Sha256)
	}
}

func TestCorruptStateSelfHeals(t *testing.T) {
	data := randData(1 << 20)
	srv := newRangeServer(t, data, srvOpts{})
	backends := testBackends(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "f.bin")
	store := mustStore(t)
	id := core.IDFor(srv.srv.URL, target)

	// Corrupt the state file, exactly like a half-written file would be.
	if err := os.WriteFile(store.Path(id), []byte(`{"id":"broken", trunc`), 0o644); err != nil {
		t.Fatal(err)
	}

	res, _ := core.Fetch(context.Background(), core.Request{
		URL: srv.srv.URL, Target: target, Resume: true, Backend: "native",
	}, store, backends)
	if !res.OK {
		t.Fatalf("a corrupt state file must not block the download: %+v", res.Error)
	}
	if res.Resumed {
		t.Errorf("nothing could be resumed from a corrupt state")
	}
	if len(res.Warnings) == 0 {
		t.Errorf("the state reset must be reported in warnings")
	}
	if res.Sha256 != digest(data) {
		t.Errorf("sha mismatch: %s", res.Sha256)
	}
	// The broken file is quarantined rather than silently destroyed.
	if _, err := os.Stat(store.Path(id) + ".corrupt"); err != nil {
		t.Errorf("corrupt state file should be quarantined: %v", err)
	}

	// The reset notice is delivered once; later cached fetches must not
	// keep repeating it.
	res2, _ := core.Fetch(context.Background(), core.Request{
		URL: srv.srv.URL, Target: target, Resume: true, Backend: "native",
	}, store, backends)
	if !res2.OK || !res2.Cached {
		t.Fatalf("expected the cached fast path, got %+v", res2)
	}
	if len(res2.Warnings) != 0 {
		t.Errorf("the reset notice must not repeat on later fetches: %v", res2.Warnings)
	}
}

func TestFetchCompletedFastPathAndCache(t *testing.T) {
	data := randData(1 << 20)
	srv := newRangeServer(t, data, srvOpts{})
	backends := testBackends(t)
	target := filepath.Join(t.TempDir(), "f.bin")

	req := core.Request{URL: srv.srv.URL, Target: target, Resume: true}
	if res, _ := core.Fetch(context.Background(), req, mustStore(t), backends); !res.OK {
		t.Fatalf("first fetch failed: %+v", res.Error)
	}
	res, _ := core.Fetch(context.Background(), req, mustStore(t), backends)
	if !res.OK || !res.Cached {
		t.Fatalf("expected cached fast path, got %+v", res)
	}
	// With the right checksum it must still pass.
	res, _ = core.Fetch(context.Background(), core.Request{
		URL: srv.srv.URL, Target: target, Resume: true, Backend: "native", Checksum: digest(data),
	}, mustStore(t), backends)
	if !res.OK {
		t.Fatalf("cached fetch with checksum failed: %+v", res.Error)
	}
}
