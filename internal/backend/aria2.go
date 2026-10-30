package backend

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"opendrain/internal/core"
)

// Aria2 wraps aria2c over JSON-RPC: it spawns one aria2c process per
// download (bound to our lifetime via --stop-with-process), hands it the
// probe-ranked mirror list, and polls tellStatus into the shared state file.
type Aria2 struct {
	Bin      string // path to aria2c; empty unless found or injected
	Endpoint string // external RPC endpoint (tests); skips spawning a process
	Secret   string
}

func NewAria2() *Aria2 {
	a := &Aria2{}
	if p, err := exec.LookPath("aria2c"); err == nil {
		a.Bin = p
	}
	return a
}

func (a *Aria2) Name() string                       { return "aria2" }
func (a *Aria2) Available(ctx context.Context) bool { return a.Endpoint != "" || a.Bin != "" }
func (a *Aria2) Status(id string) (*core.State, error) {
	return statusFromStore(id)
}
func (a *Aria2) Cancel(id string) error { return cancelByID(id) }

type aria2Status struct {
	Gid             string `json:"gid"`
	Status          string `json:"status"`
	TotalLength     string `json:"totalLength"`
	CompletedLength string `json:"completedLength"`
	ErrorCode       string `json:"errorCode"`
	ErrorMessage    string `json:"errorMessage"`
}

func (a *Aria2) Start(ctx context.Context, st *core.State, store *core.Store) error {
	urls := core.CandidateURLs(st)
	if len(urls) == 0 {
		return core.Fail(core.CodeNoMirror, "no reachable mirror", map[string]any{"probes": st.Probes})
	}
	if st.PartPath == "" {
		st.PartPath = st.Request.Target + ".part"
	}
	dir := filepath.Dir(st.PartPath)
	out := filepath.Base(st.PartPath)
	secret := a.Secret
	if secret == "" {
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		secret = hex.EncodeToString(b)
	}

	var cmd *exec.Cmd
	var exited chan error
	rpcURL := a.Endpoint
	if rpcURL == "" {
		port, err := freePort()
		if err != nil {
			return core.Fail(core.CodeInternal, "no free port: "+err.Error(), nil)
		}
		rpcURL = fmt.Sprintf("http://127.0.0.1:%d/jsonrpc", port)
		args := []string{
			"--enable-rpc=true", "--rpc-listen-all=false", "--rpc-listen-port=" + strconv.Itoa(port),
			"--rpc-secret=" + secret,
			// die together with us: no orphaned downloader left behind
			"--stop-with-process=" + strconv.Itoa(os.Getpid()),
			"--continue=true", "--allow-overwrite=true", "--auto-file-renaming=false",
			"--file-allocation=none", "--max-connection-per-server=8", "--split=8",
			"--min-split-size=1M", "--summary-interval=0", "--quiet=true",
			"--user-agent=" + core.UserAgent,
		}
		// aria2c's bundled CA lookup often fails on macOS; hand it the system
		// trust store explicitly (verification stays ON).
		for _, ca := range []string{"/etc/ssl/cert.pem", "/etc/ssl/certs/ca-certificates.crt"} {
			if _, err := os.Stat(ca); err == nil {
				args = append(args, "--ca-certificate="+ca)
				break
			}
		}
		cmd = exec.CommandContext(ctx, a.Bin, args...)
		if logf, lerr := os.Create(filepath.Join(core.StateDir(), "aria2-"+shortID(st.ID)+".log")); lerr == nil {
			cmd.Stdout, cmd.Stderr = logf, logf
			defer logf.Close()
		} else {
			cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		}
		if err := cmd.Start(); err != nil {
			return core.Fail(core.CodeBackend, "cannot start aria2c: "+err.Error(), nil)
		}
		exited = make(chan error, 1)
		go func() { exited <- cmd.Wait() }()
		defer func() {
			_, _ = a.rpc(rpcURL, secret, "aria2.shutdown")
			select {
			case <-exited:
			case <-time.After(3 * time.Second):
				if cmd.Process != nil {
					_ = cmd.Process.Kill()
					<-exited
				}
			}
		}()

		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := a.rpc(rpcURL, secret, "aria2.getVersion"); err == nil {
				break
			}
			select {
			case e := <-exited:
				return core.Fail(core.CodeBackend, "aria2c exited during startup: "+fmt.Sprint(e), nil)
			case <-ctx.Done():
				return core.Fail(core.CodeCancelled, "cancelled while starting aria2c", nil)
			case <-time.After(150 * time.Millisecond):
			}
			if time.Now().After(deadline) {
				return core.Fail(core.CodeBackend, "aria2 RPC did not become ready in 10s", nil)
			}
		}
	}

	gid, err := a.rpcString(rpcURL, secret, "aria2.addUri", urls, map[string]string{
		"dir": dir, "out": out, "continue": "true",
	})
	if err != nil {
		return core.Fail(core.CodeBackend, "aria2.addUri failed: "+err.Error(), nil)
	}

	t := time.NewTicker(300 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			_, _ = a.rpc(rpcURL, secret, "aria2.remove", gid)
			return core.Fail(core.CodeCancelled, "cancelled; state kept on disk, re-call to resume", nil)
		case <-t.C:
			raw, err := a.rpc(rpcURL, secret, "aria2.tellStatus", gid)
			if err != nil {
				if exited != nil {
					select {
					case e := <-exited:
						return core.Fail(core.CodeBackend, "aria2c exited mid-download: "+fmt.Sprint(e), nil)
					default:
					}
				}
				continue // transient RPC hiccup
			}
			var s aria2Status
			if err := json.Unmarshal(raw, &s); err != nil {
				continue
			}
			cp := *st
			cp.Status = core.StatusDownloading
			if v, e := strconv.ParseInt(s.CompletedLength, 10, 64); e == nil {
				cp.Done = v
			}
			if v, e := strconv.ParseInt(s.TotalLength, 10, 64); e == nil && v > 0 {
				cp.Total = v
			}
			_ = store.Put(&cp)
			switch s.Status {
			case "complete":
				return nil
			case "error":
				return core.Fail(core.CodeBackend, "aria2 error: "+s.ErrorMessage,
					map[string]any{"aria2_error_code": s.ErrorCode})
			case "removed":
				return core.Fail(core.CodeCancelled, "download removed", nil)
			}
		}
	}
}

func (a *Aria2) rpc(endpoint, secret, method string, params ...any) (json.RawMessage, error) {
	all := make([]any, 0, len(params)+1)
	all = append(all, "token:"+secret)
	all = append(all, params...)
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": "opendrain", "method": method, "params": all,
	})
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(endpoint, "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, err
	}
	if envelope.Error != nil {
		return nil, fmt.Errorf("rpc %s: code %d: %s", method, envelope.Error.Code, envelope.Error.Message)
	}
	return envelope.Result, nil
}

func (a *Aria2) rpcString(endpoint, secret, method string, params ...any) (string, error) {
	raw, err := a.rpc(endpoint, secret, method, params...)
	if err != nil {
		return "", err
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", err
	}
	return s, nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
