package backend

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// rangeServer serves data with RFC7233 Range support; knobs let tests inject
// faults (throttling, connection aborts, ignored Range headers).
type rangeServer struct {
	srv         *httptest.Server
	requests    atomic.Int64
	slowUntil   atomic.Int64 // unix-nano: throttle responses until this time
	ignoreRange atomic.Bool
}

type srvOpts struct {
	// abortAfterBytes: if >0, the first abortFirst connections are severed
	// after this many body bytes (0 = never).
	abortAfterBytes int64
	abortFirst      int
	// slowFor: throttle to ~64KB/20ms for this duration from server creation
	slowFor time.Duration
	// ignoreRange: always answer 200 with the full body, ignoring Range
	ignoreRange bool
	// noLength: no Content-Length header (chunked encoding, size unknown)
	noLength bool
	// probeDelay: extra latency for the probe request only (Range: bytes=0-0).
	// Mirror ranking is latency-based, so this is how a test makes the
	// ranking deterministic instead of relying on scheduling luck.
	probeDelay time.Duration
}

func newRangeServer(tb testing.TB, data []byte, o srvOpts) *rangeServer {
	rs := &rangeServer{}
	if o.slowFor > 0 {
		rs.slowUntil.Store(time.Now().Add(o.slowFor).UnixNano())
	}
	rs.ignoreRange.Store(o.ignoreRange)
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := rs.requests.Add(1)
		spec := r.Header.Get("Range")
		if o.probeDelay > 0 && spec == "bytes=0-0" {
			time.Sleep(o.probeDelay)
		}
		slow := o.slowFor > 0 && time.Now().UnixNano() < rs.slowUntil.Load()

		if rs.ignoreRange.Load() || spec == "" {
			if !o.ignoreRange {
				w.Header().Set("Accept-Ranges", "bytes")
			}
			if !o.noLength {
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			}
			w.WriteHeader(http.StatusOK)
			writeBody(w, data, slow)
			return
		}
		start, end, ok := parseRange(spec, int64(len(data)))
		if !ok {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		if !o.noLength {
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		}
		w.WriteHeader(http.StatusPartialContent)
		if o.abortAfterBytes > 0 && (o.abortFirst == 0 || n <= int64(o.abortFirst)) {
			avail := end - start + 1
			if o.abortAfterBytes < avail {
				writeBody(w, data[start:start+o.abortAfterBytes], false)
				panic(http.ErrAbortHandler)
			}
		}
		writeBody(w, data[start:end+1], slow)
	})
	rs.srv = httptest.NewServer(mux)
	tb.Cleanup(rs.srv.Close)
	return rs
}

func writeBody(w http.ResponseWriter, body []byte, slow bool) {
	defer func() { recover() }() // client may vanish mid-write; that's the point
	const piece = 64 << 10
	if !slow {
		_, _ = w.Write(body)
		return
	}
	for i := 0; i < len(body); i += piece {
		end := i + piece
		if end > len(body) {
			end = len(body)
		}
		if _, err := w.Write(body[i:end]); err != nil {
			return
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func parseRange(spec string, size int64) (int64, int64, bool) {
	spec = strings.TrimPrefix(spec, "bytes=")
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	end := size - 1
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return 0, 0, false
		}
		if end >= size {
			end = size - 1
		}
	}
	if start > end || start >= size {
		return 0, 0, false
	}
	return start, end, true
}
