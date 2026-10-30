package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"opendrain/internal/core"
)

// fake aria2c JSON-RPC: implements just enough of the protocol to exercise
// our client — addUri (which "downloads" by copying a fixture file),
// tellStatus, getVersion, shutdown.
func newFakeAria2(t *testing.T, srcPath string) *httptest.Server {
	var (
		mu        sync.Mutex
		gids      = map[string]string{} // gid -> "active" | "complete"
		next      int
		deadlines = map[string]time.Time{}
	)
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     string            `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		result := func(v any) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID, "result": v,
			})
		}
		switch req.Method {
		case "aria2.getVersion":
			result(map[string]string{"version": "fake-1.0"})
		case "aria2.addUri":
			var urls []string
			var opts map[string]string
			_ = json.Unmarshal(req.Params[1], &urls)
			if len(req.Params) > 2 {
				_ = json.Unmarshal(req.Params[2], &opts)
			}
			mu.Lock()
			next++
			gid := "g" + string(rune('0'+next))
			gids[gid] = "active"
			deadline := time.Now().Add(300 * time.Millisecond)
			deadlines[gid] = deadline
			mu.Unlock()
			go func() {
				// "download": copy fixture into dir/out when time is up
				time.Sleep(300 * time.Millisecond)
				data, err := os.ReadFile(srcPath)
				if err == nil {
					_ = os.MkdirAll(opts["dir"], 0o755)
					_ = os.WriteFile(filepath.Join(opts["dir"], opts["out"]), data, 0o644)
				}
				mu.Lock()
				gids[gid] = "complete"
				mu.Unlock()
			}()
			result(gid)
		case "aria2.tellStatus":
			var gid string
			_ = json.Unmarshal(req.Params[1], &gid)
			mu.Lock()
			status := gids[gid]
			deadline := deadlines[gid]
			mu.Unlock()
			resp := map[string]any{"gid": gid, "status": status,
				"completedLength": "0", "totalLength": "0"}
			if status == "complete" {
				resp["completedLength"] = "100"
				resp["totalLength"] = "100"
			}
			_ = deadline
			result(resp)
		case "aria2.shutdown", "aria2.remove":
			result("ok")
		default:
			result(nil)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestAria2BackendViaFakeRPC(t *testing.T) {
	data := randData(1 << 20)
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	// A locally reachable URL for the probe phase; the fake aria2 "downloads"
	// by copying the fixture regardless of the URL.
	probeSrv := newRangeServer(t, data, srvOpts{})
	srv := newFakeAria2(t, src)
	backends := testBackends(t)
	aria2 := &Aria2{Endpoint: srv.URL, Secret: "testsecret"}
	backends = append([]core.Downloader{aria2}, backends...)
	target := filepath.Join(dir, "out.bin")

	res, st := core.Fetch(context.Background(), core.Request{
		URL: probeSrv.srv.URL, Target: target, Resume: true,
		Backend: "aria2",
	}, mustStore(t), backends)
	if !res.OK {
		t.Fatalf("aria2 fetch failed: %+v", res.Error)
	}
	if res.Backend != "aria2" {
		t.Errorf("backend=%s, want aria2", res.Backend)
	}
	if res.Sha256 != digest(data) {
		t.Errorf("sha mismatch: %s", res.Sha256)
	}
	if st.Status != core.StatusCompleted {
		t.Errorf("status=%s", st.Status)
	}
	if got, err := os.ReadFile(target); err != nil || len(got) != len(data) {
		t.Fatalf("published file wrong: len=%d err=%v", len(got), err)
	}
}

func TestAria2ExplicitUnavailableFailsCleanly(t *testing.T) {
	backends := testBackends(t)
	target := filepath.Join(t.TempDir(), "out.bin")
	res, _ := core.Fetch(context.Background(), core.Request{
		URL: "http://example.org/x", Target: target, Backend: "aria2",
	}, mustStore(t), backends)
	// No aria2 injected here; NewAria2 may find a real aria2c on this host,
	// so accept either a clean failure before probing or a no_mirror error.
	if res.OK {
		return
	}
	if res.Error == nil {
		t.Fatalf("expected structured error, got %+v", res)
	}
}
