package core

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseChecksum(t *testing.T) {
	if _, _, err := ParseChecksum("sha256:" + "aB" + string(make([]byte, 0))); err == nil {
		t.Error("short hex should fail")
	}
	good := "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	algo, hexv, err := ParseChecksum(good)
	if err != nil || algo != "sha256" || len(hexv) != 64 {
		t.Errorf("valid checksum rejected: %v %s", err, hexv)
	}
	if _, _, err := ParseChecksum("md5:abc"); err == nil {
		t.Error("md5 should be rejected")
	}
}

func TestMakeChunks(t *testing.T) {
	cs := MakeChunks(100, 4)
	if len(cs) != 4 {
		t.Fatalf("got %d chunks", len(cs))
	}
	var sum int64
	for _, c := range cs {
		sum += c.Size()
		if c.Done != 0 {
			t.Error("fresh chunk must have Done=0")
		}
	}
	if sum != 100 {
		t.Errorf("chunks cover %d bytes, want 100", sum)
	}
	if cs[0].Start != 0 || cs[3].End != 99 {
		t.Errorf("bad boundaries: %+v", cs)
	}
	// uneven division
	cs = MakeChunks(10, 4)
	if sum := chunkSum(cs); sum != 10 {
		t.Errorf("uneven chunks cover %d", sum)
	}
}

func chunkSum(cs []Chunk) int64 {
	var s int64
	for _, c := range cs {
		s += c.Size()
	}
	return s
}

func TestPutProgressIsAtomicAndReadable(t *testing.T) {
	t.Setenv("OPENDRAIN_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	s, err := NewStore()
	if err != nil {
		t.Fatal(err)
	}
	id := IDFor("http://x/big", "/tmp/big")
	if err := s.Put(&State{ID: id, Status: StatusDownloading, Done: 10, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for i := int64(11); i <= 30; i++ {
		if err := s.PutProgress(&State{ID: id, Status: StatusDownloading, Done: i, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		got, err := s.Get(id)
		if err != nil || got == nil || got.Done != i {
			t.Fatalf("progress %d lost: %v %+v", i, err, got)
		}
	}
	// no .tmp left behind, and the state file is complete JSON
	if _, err := os.Stat(s.Path(id) + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file should not remain: %v", err)
	}
}

func TestStorePutGetRoundTrip(t *testing.T) {
	t.Setenv("OPENDRAIN_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	s, err := NewStore()
	if err != nil {
		t.Fatal(err)
	}
	id := IDFor("http://x/y", "/tmp/z")
	st := &State{ID: id, Status: StatusDownloading, Total: 10, Done: 4,
		CreatedAt: time.Now(), Chunks: []Chunk{{0, 9, 4}}}
	if err := s.Put(st); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(id)
	if err != nil || got == nil {
		t.Fatalf("get after put: %v", err)
	}
	if got.Done != 4 || len(got.Chunks) != 1 || got.Chunks[0].Done != 4 {
		t.Errorf("round trip lost data: %+v", got)
	}
	if missing, _ := s.Get("nonexistent"); missing != nil {
		t.Error("missing state should be nil,nil")
	}
}

func TestProbeAllSortsFastRangeFirst(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(250 * time.Millisecond)
		w.Header().Set("Content-Range", "bytes 0-0/1000")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte("x"))
	}))
	defer slow.Close()
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-0/1000")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte("x"))
	}))
	defer fast.Close()

	// slow first: probe order must not matter for the ranking
	probes := ProbeAll(context.Background(), []string{slow.URL, fast.URL}, 5*time.Second)
	if probes[0].URL != fast.URL {
		t.Errorf("fast server should rank first, got %+v", probes)
	}
	if !probes[0].RangeOK || probes[0].Size != 1000 {
		t.Errorf("probe should see 206+size, got %+v", probes[0])
	}
}

func TestProbeDoesNotTrustAcceptRangesHeader(t *testing.T) {
	// A server that advertises byte ranges but answers every range request
	// with 200 + the full body. Our probe asked for "Range: bytes=0-0" and
	// got 200, which proves the server ignored it — the Accept-Ranges header
	// is lying. RangeOK must be false, otherwise we pick ranged mode and
	// fail every single download against such a server.
	liar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("x"))
	}))
	defer liar.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	p := ProbeOne(context.Background(), client, liar.URL)
	if p.Error != "" {
		t.Fatalf("probe failed: %v", p.Error)
	}
	if p.RangeOK {
		t.Errorf("RangeOK must be false when the server answered our range request with 200")
	}
	if p.Size != 1000 {
		t.Errorf("size=%d, want 1000", p.Size)
	}
}

func TestIDForNormalizesRelativeTargets(t *testing.T) {
	// fetch and status must agree on the state id even when the target is
	// given relative in one call and absolute in the other.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(wd, "sub", "file.bin")
	rel := filepath.Join("sub", "file.bin")
	if IDFor("http://x/y", abs) != IDFor("http://x/y", rel) {
		t.Errorf("relative and absolute targets must map to the same state id")
	}
	if IDFor("http://x/y", abs) == IDFor("http://x/z", abs) {
		t.Errorf("different urls must map to different state ids")
	}
}

func TestFileSha256KnownVector(t *testing.T) {
	p := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := FileSha256(p)
	if err != nil {
		t.Fatal(err)
	}
	want := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != want {
		t.Errorf("sha256(\"\")=%s, want %s", got, want)
	}
}

func TestManifestReportsManagedFilesAndDiskState(t *testing.T) {
	t.Setenv("OPENDRAIN_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	s, err := NewStore()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	present := filepath.Join(dir, "present.txt")
	missing := filepath.Join(dir, "missing.txt")
	if err := os.WriteFile(present, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	for _, tc := range []struct {
		target string
		sum    string
	}{
		{present, sum},
		{present, "sha256:" + strings.Repeat("0", 64)},
		{missing, sum},
	} {
		id := IDFor("http://x/"+filepath.Base(tc.target)+tc.sum, tc.target)
		if err := s.Put(&State{ID: id, Status: StatusCompleted, Total: 5, Sha256: tc.sum,
			Request:   Request{URL: "http://x/" + filepath.Base(tc.target), Target: tc.target},
			CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	m, err := s.Manifest(true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if m.Count != 3 || m.OnDisk != 2 {
		t.Errorf("count=%d on_disk=%d, want 3/2", m.Count, m.OnDisk)
	}
	verified, mismatched, nofile := 0, 0, 0
	for _, e := range m.Entries {
		if e.Verified != nil && *e.Verified {
			verified++
		}
		if e.Verified != nil && !*e.Verified {
			mismatched++
		}
		if !e.OnDisk {
			nofile++
		}
	}
	if verified != 1 || mismatched != 1 || nofile != 1 {
		t.Errorf("verified=%d mismatched=%d missing=%d, want 1/1/1", verified, mismatched, nofile)
	}
	found, err := s.FindByTarget(present)
	if err != nil || found == nil {
		t.Fatalf("FindByTarget failed: %v", err)
	}
	if none, err := s.FindByTarget(filepath.Join(dir, "never.txt")); err != nil || none != nil {
		t.Errorf("unknown target should be nil,nil: %v %v", none, err)
	}
}

func TestReadFileSliceModesAndBound(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "doc.txt")
	body := "alpha one\nbeta target line\ncontext after\nfar away unrelated\ngamma\nlast target\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	grepped, err := ReadFileSlice(p, ReadOptions{Pattern: "target", Context: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(grepped, "alpha one") || !strings.Contains(grepped, "context after") ||
		!strings.Contains(grepped, "gamma") || !strings.Contains(grepped, "last target") ||
		strings.Contains(grepped, "far away unrelated") {
		t.Errorf("grep excerpt wrong: %q", grepped)
	}

	head, err := ReadFileSlice(p, ReadOptions{Head: 2})
	if err != nil || !strings.HasPrefix(head, "alpha one\nbeta") {
		t.Errorf("head wrong: %q %v", head, err)
	}
	tail, err := ReadFileSlice(p, ReadOptions{Tail: 1})
	if err != nil || tail != "last target\n" {
		t.Errorf("tail wrong: %q %v", tail, err)
	}
	bounded, err := ReadFileSlice(p, ReadOptions{MaxBytes: 9})
	if err != nil || len(bounded) != 9 {
		t.Errorf("max bytes not respected: %q (%d) %v", bounded, len(bounded), err)
	}
	if _, err := ReadFileSlice(p, ReadOptions{Pattern: "["}); err == nil {
		t.Error("invalid regex should fail")
	}
}

func TestTaskLockSerializesAndReleases(t *testing.T) {
	t.Setenv("OPENDRAIN_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	s, err := NewStore()
	if err != nil {
		t.Fatal(err)
	}
	release, err := s.TaskLock(context.Background(), "task-a")
	if err != nil {
		t.Fatal(err)
	}

	// A second caller for the same task must not proceed while the lock is
	// held; it gives up when its context expires rather than deadlocking.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := s.TaskLock(ctx, "task-a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded while locked, got %v", err)
	}

	// A different task is unaffected — locks are per (url, target).
	relOther, err := s.TaskLock(context.Background(), "task-b")
	if err != nil {
		t.Errorf("unrelated task should not be blocked: %v", err)
	} else {
		relOther()
	}

	release()
	relAgain, err := s.TaskLock(context.Background(), "task-a")
	if err != nil {
		t.Fatalf("lock not released after release(): %v", err)
	}
	relAgain()
}

func TestProbeReusableOnlyForFreshHealthySameURLSet(t *testing.T) {
	now := time.Now()
	good := []ProbeInfo{{URL: "http://x/a", Status: 206, RangeOK: true}}
	st := &State{Probes: good, ProbedAt: now}
	if !probeReusable(st, []string{"http://x/a"}) {
		t.Error("fresh healthy probe for the same URL should be reusable")
	}
	if probeReusable(st, []string{"http://x/b"}) {
		t.Error("a different URL must not reuse another URL's probe")
	}
	if probeReusable(st, []string{"http://x/a", "http://x/b"}) {
		t.Error("a changed mirror set must not reuse stale probes")
	}

	dead := &State{Probes: []ProbeInfo{{URL: "http://x/a", Error: "timeout"}}, ProbedAt: now}
	if probeReusable(dead, []string{"http://x/a"}) {
		t.Error("failed probe must never be reused")
	}
	stale := &State{Probes: good, ProbedAt: now.Add(-2 * time.Minute)}
	if probeReusable(stale, []string{"http://x/a"}) {
		t.Error("probe older than 60s must not be reused")
	}
	dup := &State{Probes: append(append([]ProbeInfo{}, good...), good...), ProbedAt: now}
	if probeReusable(dup, []string{"http://x/a", "http://x/a"}) {
		t.Error("duplicate probe URLs must not be treated as healthy")
	}
}

func TestCachedChecksumMismatchQuarantinesInsteadOfDeleting(t *testing.T) {
	t.Setenv("OPENDRAIN_STATE_DIR", filepath.Join(t.TempDir(), "state"))
	s, err := NewStore()
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "cached.bin")
	if err := os.WriteFile(target, []byte("precious bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	req := Request{URL: "http://example.invalid/f", Target: target, Resume: true}
	id := IDFor(req.URL, req.Target)
	if err := s.Put(&State{ID: id, Status: StatusCompleted, Request: req, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	res, _ := Fetch(context.Background(), Request{
		URL: req.URL, Target: target, Resume: true, Backend: "native",
		Checksum: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
	}, s, nil)
	if res.OK || res.Error == nil || res.Error.Code != CodeChecksum {
		t.Fatalf("expected checksum_mismatch, got %+v", res.Error)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("original target should no longer be at its path: %v", err)
	}
	corrupt := target + ".corrupt"
	got, err := os.ReadFile(corrupt)
	if err != nil || string(got) != "precious bytes" {
		t.Fatalf("quarantined file missing or changed: %q %v", got, err)
	}
	if res.Error.Detail["quarantined_as"] != corrupt {
		t.Errorf("detail should report the quarantine path, got %v", res.Error.Detail)
	}

	// A second mismatch must not overwrite the first piece of evidence.
	if err := os.WriteFile(target, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(&State{ID: id, Status: StatusCompleted, Request: req, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	res2, _ := Fetch(context.Background(), Request{
		URL: req.URL, Target: target, Resume: true, Backend: "native",
		Checksum: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
	}, s, nil)
	if res2.Error == nil || res2.Error.Code != CodeChecksum {
		t.Fatalf("second mismatch failed: %+v", res2.Error)
	}
	if first, err := os.ReadFile(corrupt); err != nil || string(first) != "precious bytes" {
		t.Errorf("first quarantined file was overwritten: %q %v", first, err)
	}
	matches, _ := filepath.Glob(target + ".corrupt-*")
	if len(matches) != 1 {
		t.Errorf("expected a timestamped second quarantine file, got %v", matches)
	}
}
