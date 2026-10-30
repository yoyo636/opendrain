package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"opendrain/internal/core"
)

const (
	stallTimeout   = 30 * time.Second
	attemptsPerURL = 3
	maxConns       = 6
	minRangedSize  = 8 << 20
	targetChunk    = 16 << 20
)

// Native is the zero-dependency fallback downloader: concurrent Range
// requests, per-chunk progress persisted for resume, mirror failover with
// already-downloaded bytes kept.
type Native struct {
	Client *http.Client
}

func NewNative() *Native {
	return &Native{Client: &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   maxConns,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}}
}

func (n *Native) Name() string                       { return "native" }
func (n *Native) Available(ctx context.Context) bool { return true }
func (n *Native) Status(id string) (*core.State, error) {
	store, err := core.NewStore()
	if err != nil {
		return nil, err
	}
	return store.Get(id)
}
func (n *Native) Cancel(id string) error { return cancelByID(id) }

type dlPlan struct {
	mode   string
	total  int64 // -1 = unknown
	chunks []core.Chunk
}

func (n *Native) Start(ctx context.Context, st *core.State, store *core.Store) error {
	urls := core.CandidateURLs(st)
	if len(urls) == 0 {
		return core.Fail(core.CodeNoMirror, "no reachable mirror", map[string]any{"probes": st.Probes})
	}
	partPath := st.PartPath
	if partPath == "" {
		partPath = st.Request.Target + ".part"
		st.PartPath = partPath
	}
	f, err := os.OpenFile(partPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return core.Fail(core.CodeInternal, "cannot open part file: "+err.Error(), nil)
	}
	defer f.Close()

	plan, perr := planDownload(ctx, st, f)
	if perr != nil {
		return perr
	}

	prog := newProgress(plan.chunks)
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	trackCancel(st.ID, cancel)

	errMu := &sync.Mutex{}
	errs := make([]error, len(plan.chunks))
	var wg sync.WaitGroup
	for i := range plan.chunks {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := n.runChunk(gctx, f, i, plan, urls, prog)
			errMu.Lock()
			errs[i] = e
			errMu.Unlock()
			if e != nil {
				cancel()
			}
		}(i)
	}

	done := make(chan struct{})
	var saverWG sync.WaitGroup
	saverWG.Add(1)
	go func() {
		defer saverWG.Done()
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-gctx.Done():
				return
			case <-t.C:
				saveProgress(st, store, prog, plan)
			}
		}
	}()

	wg.Wait()
	cancel() // in case it's still set
	close(done)
	saverWG.Wait()
	saveProgress(st, store, prog, plan) // final authoritative snapshot

	if ctx.Err() != nil {
		return core.Fail(core.CodeCancelled, "download cancelled; state kept on disk, re-call to resume", nil)
	}
	var realErr error
	for _, e := range errs {
		if e != nil && !errors.Is(e, context.Canceled) {
			realErr = e
			break
		}
	}
	if realErr != nil {
		return core.Fail(core.CodeAllFailed, "chunk failed after all mirrors/attempts: "+realErr.Error(), nil)
	}
	got, _ := prog.snapshot()
	if plan.total >= 0 && got != plan.total {
		return core.Fail(core.CodeInternal, fmt.Sprintf("byte count mismatch after download: %d/%d", got, plan.total), nil)
	}

	st.Mode = plan.mode
	st.Total = plan.total
	if st.Total < 0 {
		st.Total = 0
	}
	st.Done = got
	st.Chunks = plan.chunks
	for i := range st.Chunks {
		st.Chunks[i].Done = st.Chunks[i].Size()
	}
	return nil
}

// planDownload decides ranged-parallel vs single-stream, honoring whatever
// resumable progress is recorded in the state (and on disk).
func planDownload(ctx context.Context, st *core.State, f *os.File) (dlPlan, error) {
	var probe *core.ProbeInfo
	for i := range st.Probes {
		if st.Probes[i].Error == "" {
			probe = &st.Probes[i]
			break
		}
	}
	if probe == nil {
		return dlPlan{}, core.Fail(core.CodeNoMirror, "no reachable mirror", nil)
	}
	total := probe.Size // -1 = unknown
	ranged := probe.RangeOK
	partSize := fileStats(f)

	fresh := func() (dlPlan, error) {
		if total < 0 || !ranged {
			st.Mode, st.Chunks, st.Total, st.Done = core.ModeSingle, nil, 0, 0
			if total < 0 {
				st.Total = 0
			} else {
				st.Total = total
			}
			_ = f.Truncate(0)
			var end int64 = -1
			if total >= 0 {
				end = total - 1
			}
			return dlPlan{mode: core.ModeSingle, total: total, chunks: []core.Chunk{{Start: 0, End: end}}}, nil
		}
		n := int(total / targetChunk)
		if total >= minRangedSize {
			if n > 8 {
				n = 8
			}
			if n < 2 {
				n = 2
			}
		} else {
			n = 1
		}
		chunks := makeChunks(total, n)
		st.Mode, st.Chunks, st.Total, st.Done = core.ModeRanged, chunks, total, 0
		_ = f.Truncate(total)
		return dlPlan{mode: core.ModeRanged, total: total, chunks: chunks}, nil
	}

	if st.Mode == core.ModeRanged && len(st.Chunks) > 0 && total >= 0 && st.Total == total && partSize == total {
		// resume a previous ranged download
		return dlPlan{mode: core.ModeRanged, total: total, chunks: st.Chunks}, nil
	}
	if st.Mode == core.ModeSingle && len(st.Chunks) == 1 && st.Done > 0 &&
		partSize == st.Done && (total < 0 || st.Total == total || st.Total == 0) {
		// resume a previous single-stream download
		c := st.Chunks[0]
		if total >= 0 {
			c.End = total - 1
		}
		return dlPlan{mode: core.ModeSingle, total: total, chunks: []core.Chunk{c}}, nil
	}
	return fresh()
}

func (n *Native) runChunk(ctx context.Context, f *os.File, idx int, plan dlPlan, urls []string, prog *progress) error {
	ch := plan.chunks[idx]
	unknown := ch.End < 0
	whole := ch.Start == 0 && (unknown || ch.End == plan.total-1)
	maxAttempts := len(urls) * attemptsPerURL
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		done := prog.chunkDone(idx)
		if !unknown && done >= ch.Size() {
			return nil
		}
		url := urls[attempt%len(urls)]
		body, status, err := n.open(ctx, url, ch.Start+done, ch.End)
		if err != nil {
			lastErr = err
			continue
		}
		acceptable := status == http.StatusPartialContent ||
			(status == http.StatusOK && whole && done == 0)
		if !acceptable {
			_, _ = io.Copy(io.Discard, io.LimitReader(body, 1024))
			body.Close()
			lastErr = fmt.Errorf("%s returned http %d for range request (start=%d)", url, status, ch.Start+done)
			continue
		}
		effDone := done
		if status == http.StatusOK && done > 0 {
			// server ignored Range; restart this chunk from zero
			prog.setChunk(idx, 0)
			if unknown {
				_ = f.Truncate(0)
			}
			effDone = 0
		}
		start := ch.Start + effDone
		remaining := int64(-1)
		if !unknown {
			remaining = ch.End - start + 1
		}
		written, werr := copyChunk(f, body, start, remaining, prog, idx)
		body.Close()
		if werr != nil {
			if errors.Is(werr, errFatal) {
				return werr
			}
			lastErr = werr
			continue
		}
		if unknown {
			if written == 0 {
				lastErr = errors.New("server sent an empty body")
				continue
			}
			return nil // EOF reached
		}
		if effDone+written == ch.Size() {
			return nil
		}
		lastErr = fmt.Errorf("short read: %d/%d bytes", effDone+written, ch.Size())
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return lastErr
}

func (n *Native) open(ctx context.Context, url string, start, end int64) (io.ReadCloser, int, error) {
	cctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		return nil, 0, err
	}
	req.Header.Set("User-Agent", core.UserAgent)
	req.Header.Set("Accept-Encoding", "identity")
	if start > 0 || end >= 0 {
		if end >= 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		} else {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
		}
	}
	resp, err := n.Client.Do(req)
	if err != nil {
		cancel()
		return nil, 0, err
	}
	if resp.StatusCode/100 != 2 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		resp.Body.Close()
		cancel()
		return nil, resp.StatusCode, fmt.Errorf("http %d: %s", resp.StatusCode, truncateSnippet(snippet))
	}
	if resp.Header.Get("Content-Encoding") != "" && resp.StatusCode == http.StatusPartialContent {
		resp.Body.Close()
		cancel()
		return nil, 0, errors.New("server applied content-encoding to a range response")
	}
	// A body that goes silent for stallTimeout means a dead connection:
	// cancel the attempt so the retry loop can pick the next mirror.
	return newStallBody(resp.Body, cancel), resp.StatusCode, nil
}

func truncateSnippet(b []byte) string {
	if len(b) > 200 {
		b = b[:200]
	}
	return string(b)
}

var errFatal = errors.New("fatal")

func copyChunk(f *os.File, body io.Reader, start, remaining int64, prog *progress, idx int) (int64, error) {
	buf := make([]byte, 128<<10)
	var written int64
	for remaining < 0 || written < remaining {
		toRead := len(buf)
		if remaining > 0 && int64(toRead) > remaining-written {
			toRead = int(remaining - written)
		}
		nr, er := body.Read(buf[:toRead])
		if nr > 0 {
			if _, we := f.WriteAt(buf[:nr], start+written); we != nil {
				return written, fmt.Errorf("%w: writing part file: %v", errFatal, we)
			}
			written += int64(nr)
			prog.addChunk(idx, int64(nr))
		}
		if er == io.EOF {
			return written, nil
		}
		if er != nil {
			return written, er
		}
	}
	return written, nil
}

type stallBody struct {
	io.ReadCloser
	cancel context.CancelFunc
	t      *time.Timer
}

func newStallBody(rc io.ReadCloser, cancel context.CancelFunc) *stallBody {
	sb := &stallBody{ReadCloser: rc, cancel: cancel}
	sb.t = time.AfterFunc(stallTimeout, sb.cancel)
	return sb
}

func (s *stallBody) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	if n > 0 {
		s.t.Reset(stallTimeout)
	}
	return n, err
}

func (s *stallBody) Close() error {
	s.t.Stop()
	return s.ReadCloser.Close()
}

type progress struct {
	mu   sync.Mutex
	done []int64
}

func newProgress(chunks []core.Chunk) *progress {
	p := &progress{done: make([]int64, len(chunks))}
	for i, c := range chunks {
		p.done[i] = c.Done
	}
	return p
}

func (p *progress) chunkDone(i int) int64 { p.mu.Lock(); defer p.mu.Unlock(); return p.done[i] }
func (p *progress) addChunk(i int, n int64) {
	p.mu.Lock()
	p.done[i] += n
	p.mu.Unlock()
}
func (p *progress) setChunk(i int, v int64) {
	p.mu.Lock()
	p.done[i] = v
	p.mu.Unlock()
}
func (p *progress) snapshot() (int64, []int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]int64, len(p.done))
	copy(out, p.done)
	var t int64
	for _, v := range out {
		t += v
	}
	return t, out
}

func saveProgress(st *core.State, store *core.Store, prog *progress, plan dlPlan) {
	done, dn := prog.snapshot()
	cp := *st
	cp.Status = core.StatusDownloading
	cp.Done = done
	if plan.total > 0 {
		cp.Total = plan.total
	}
	if len(plan.chunks) > 0 {
		cs := make([]core.Chunk, len(plan.chunks))
		copy(cs, plan.chunks)
		for i := range cs {
			cs[i].Done = dn[i]
		}
		cp.Chunks = cs
	} else {
		cp.Chunks = nil
	}
	_ = store.Put(&cp)
}

func makeChunks(total int64, n int) []core.Chunk { return core.MakeChunks(total, n) }

func fileSize(p string) int64 {
	if fi, err := os.Stat(p); err == nil {
		return fi.Size()
	}
	return -1
}

func fileStats(f *os.File) int64 {
	if fi, err := f.Stat(); err == nil {
		return fi.Size()
	}
	return -1
}
