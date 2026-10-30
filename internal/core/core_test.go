package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
