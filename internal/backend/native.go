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
	attemptsPerURL = 3
	minRangedSize  = 8 << 20
	// Concurrency and chunk granularity are separate concerns. Splitting a
	// 1GB file into exactly 16 ranges gives each connection a 64MB tail, so
	// one jittery connection dominates the whole download and resume loses at
	// most 64MB. Instead, cut the file into many small work units and let a
	// fixed pool of workers pull them: a slow connection only delays one unit,
	// fast workers drain the rest, and resume granularity is much finer.
	targetChunk       = 2 << 20  // used to derive the worker count
	minChunkSize      = 2 << 20  // never create request overhead for <2MB
	maxChunkSize      = 16 << 20 // ~12 units per worker on a 3GB file
	chunksPerWorker   = 4        // work units per worker, for load balance
	maxWorkChunks     = 4096     // sanity cap on persisted chunk count
	maxChunks         = 16       // simultaneous connections
	copyBufferSize    = 512 << 10
	githubSmallChunks = 4
	githubLargeSize   = 256 << 20
	// Accumulate progress locally and publish to the shared counter in
	// batches: a 1GiB/512KB download otherwise takes 2048 contended locks.
	progressBatch = 4 << 20
	// Progress snapshots do not need fsync or 500ms granularity on big files.
	smallProgressInterval = 500 * time.Millisecond
	largeProgressInterval = 2 * time.Second
)

// Liveness thresholds are variables so tests can shorten the window. A
// connection that trickles a byte every few seconds used to keep the 30s
// inactivity timer alive forever; the rate window catches that.
var (
	stallTimeout = 30 * time.Second
	rateWindow   = 10 * time.Second
	minReadRate  = int64(8 << 10) // 8 KiB/s sustained; below this is dead
)

// Native is the zero-dependency fallback downloader: concurrent Range
// requests, per-chunk progress persisted for resume, mirror failover with
// already-downloaded bytes kept.
type Native struct {
	Client *http.Client
}

func NewNative() *Native {
	base := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   maxChunks,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	return &Native{Client: &http.Client{
		Transport:     core.WrapGitHubAuth(base),
		CheckRedirect: core.SafeRedirectPolicy,
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
	mode    string
	total   int64 // -1 = unknown
	chunks  []core.Chunk
	workers int
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

	workers := plan.workers
	if workers < 1 {
		workers = 1
	}
	if workers > len(plan.chunks) {
		workers = len(plan.chunks)
	}

	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	work := make(chan int)
	fail := func(e error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = e
		}
		errMu.Unlock()
		cancel()
	}
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range work {
				if gctx.Err() != nil {
					return
				}
				if e := n.runChunk(gctx, f, idx, plan, urls, prog); e != nil {
					fail(e)
					return
				}
			}
		}()
	}
	// Feeder: skip chunks already complete from a previous session, hand the
	// rest to whichever worker is free. Unbuffered so a cancelled download
	// never blocks closing the channel.
	go func() {
		defer close(work)
		for i := range plan.chunks {
			ch := plan.chunks[i]
			if ch.Size() >= 0 && prog.chunkDone(i) >= ch.Size() {
				continue
			}
			select {
			case work <- i:
			case <-gctx.Done():
				return
			}
		}
	}()

	done := make(chan struct{})
	var saverWG sync.WaitGroup
	saverWG.Add(1)
	go func() {
		defer saverWG.Done()
		interval := smallProgressInterval
		if plan.total >= githubLargeSize {
			interval = largeProgressInterval
		}
		t := time.NewTicker(interval)
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
	errMu.Lock()
	realErr := firstErr
	errMu.Unlock()
	if realErr != nil && !errors.Is(realErr, context.Canceled) {
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
		if st.Chunks[i].Size() >= 0 {
			st.Chunks[i].Done = st.Chunks[i].Size()
		} else {
			st.Chunks[i].Done = st.Total
		}
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
	// GitHub's signed CDN redirects showed highly unstable per-connection
	// throughput; cap the worker count for SMALL GitHub files while letting
	// GB-scale assets use the full pool. Chunk granularity is handled below.
	github := core.GitHubThrottled(st.Request.URL)
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
			return dlPlan{mode: core.ModeSingle, total: total,
				chunks: []core.Chunk{{Start: 0, End: end}}, workers: 1}, nil
		}
		workers := workerCount(total, github)
		chunks := rangedChunks(total, workers)
		st.Mode, st.Chunks, st.Total, st.Done = core.ModeRanged, chunks, total, 0
		_ = f.Truncate(total)
		return dlPlan{mode: core.ModeRanged, total: total, chunks: chunks, workers: workers}, nil
	}

	if st.Mode == core.ModeRanged && len(st.Chunks) > 0 && total >= 0 && st.Total == total && partSize == total {
		// Resume a previous ranged download. Existing states may carry fewer
		// (larger) chunks from older versions; keep them rather than throwing
		// away completed work, and cap the worker count to the chunk count.
		workers := workerCount(total, github)
		if workers > len(st.Chunks) {
			workers = len(st.Chunks)
		}
		if workers < 1 {
			workers = 1
		}
		return dlPlan{mode: core.ModeRanged, total: total, chunks: st.Chunks, workers: workers}, nil
	}
	// Single-stream and unknown-size downloads cannot be resumed reliably:
	// a server that ignores Range has no way to continue from an offset. A
	// fresh start is the only honest option (Fetch reports resumed=false).
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
			(status == http.StatusOK && whole)
		if !acceptable {
			_, _ = io.Copy(io.Discard, io.LimitReader(body, 1024))
			body.Close()
			lastErr = fmt.Errorf("%s returned http %d for range request (start=%d)", url, status, ch.Start+done)
			continue
		}
		effDone := done
		if status == http.StatusOK && done > 0 {
			// Server ignored Range. Since this is the whole file, restart from
			// zero: truncate and rewind progress. Previously this branch was
			// unreachable (acceptable rejected 200 when done>0), so a single
			// interrupted non-Range download could never finish.
			prog.setChunk(idx, 0)
			_ = f.Truncate(0)
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
	buf := make([]byte, copyBufferSize)
	var written int64
	var pending int64 // not yet published to the shared progress counter
	publish := func() {
		if pending > 0 {
			prog.addChunk(idx, pending)
			pending = 0
		}
	}
	defer publish() // never lose the tail on a clean return
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
			pending += int64(nr)
			if pending >= progressBatch {
				publish()
			}
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
	cancel      context.CancelFunc
	t           *time.Timer
	windowStart time.Time
	windowBytes int64
}

func newStallBody(rc io.ReadCloser, cancel context.CancelFunc) *stallBody {
	sb := &stallBody{ReadCloser: rc, cancel: cancel, windowStart: time.Now()}
	sb.t = time.AfterFunc(stallTimeout, sb.abort)
	return sb
}

// abort closes the body as well as cancelling the request context. Closing is
// what actually unblocks a goroutine already parked in a socket read; context
// cancellation alone was not enough on every platform/path.
func (s *stallBody) abort() {
	s.cancel()
	_ = s.ReadCloser.Close()
}

func (s *stallBody) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	if n > 0 {
		s.windowBytes += int64(n)
		s.t.Reset(stallTimeout)
		now := time.Now()
		if elapsed := now.Sub(s.windowStart); elapsed >= rateWindow {
			rate := float64(s.windowBytes) / elapsed.Seconds()
			if rate < float64(minReadRate) {
				s.abort()
				return n, fmt.Errorf("download stalled: %.0f B/s is below %d B/s", rate, minReadRate)
			}
			s.windowStart, s.windowBytes = now, 0
		}
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
	_ = store.PutProgress(&cp)
}

// workerCount decides simultaneous connections from file size and host. Kept
// pure so the large-file / GitHub small-file rules can be tested offline.
func workerCount(total int64, github bool) int {
	if total < minRangedSize {
		return 1
	}
	// round up so a 28MB file gets 14 workers instead of 2
	n := int((total + targetChunk - 1) / targetChunk)
	if n > maxChunks {
		n = maxChunks
	}
	if n < 2 {
		n = 2
	}
	// never create sub-2MB work units: request overhead would eat the gain
	if byMin := int(total / minChunkSize); n > byMin && byMin > 1 {
		n = byMin
	}
	// GitHub's CDN is jittery for small files (16-way was slower), but a
	// GB-scale asset needs full concurrency — a 4-way 1GB download is minutes.
	if github && total < githubLargeSize && n > githubSmallChunks {
		n = githubSmallChunks
	}
	return n
}

// rangedChunks cuts the target into many small work units for the worker pool.
// Old versions created exactly workerCount chunks, so a 3GB download had 16
// ~193MB tails; a single jittery connection then added minutes to the end.
// With ~2-16MB units a slow connection only delays one small piece.
func rangedChunks(total int64, workers int) []core.Chunk {
	if total <= 0 {
		return nil
	}
	size := int64(maxChunkSize)
	if workers > 1 {
		size = total / int64(workers*chunksPerWorker)
	}
	if size < minChunkSize {
		size = minChunkSize
	}
	if size > maxChunkSize {
		size = maxChunkSize
	}
	n := int((total + size - 1) / size)
	if n < 1 {
		n = 1
	}
	if n > maxWorkChunks {
		n = maxWorkChunks
	}
	return core.MakeChunks(total, n)
}

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
