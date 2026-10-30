package core

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const UserAgent = "opendrain/0.1"

// ProbeAll measures every candidate in parallel with one cheap request
// (GET + Range: bytes=0-0): it yields time-to-first-byte, whether the server
// honors byte ranges (HTTP 206), and the total size from Content-Range.
// Results are sorted best-first: reachable > range-capable > lowest latency;
// stable sort keeps the caller's primary URL first on ties.
func ProbeAll(ctx context.Context, urls []string, timeout time.Duration) []ProbeInfo {
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: timeout,
			MaxIdleConns:          len(urls) + 1,
		},
	}
	results := make([]ProbeInfo, len(urls))
	var wg sync.WaitGroup
	for i, u := range urls {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			results[i] = ProbeOne(ctx, client, u)
		}(i, u)
	}
	wg.Wait()

	out := make([]ProbeInfo, len(results))
	copy(out, results)
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if (a.Error == "") != (b.Error == "") {
			return b.Error != ""
		}
		if a.RangeOK != b.RangeOK {
			return a.RangeOK
		}
		return a.LatencyMS < b.LatencyMS
	})
	return out
}

func ProbeOne(ctx context.Context, client *http.Client, url string) ProbeInfo {
	p := ProbeInfo{URL: url, Size: -1}
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		p.Error = err.Error()
		return p
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Range", "bytes=0-0")
	resp, err := client.Do(req)
	p.LatencyMS = time.Since(start).Milliseconds()
	if err != nil {
		p.Error = err.Error()
		return p
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, 1) // confirm data actually flows

	p.Status = resp.StatusCode
	switch {
	case resp.StatusCode == http.StatusPartialContent:
		p.RangeOK = true
		p.Size = parseContentRangeTotal(resp.Header.Get("Content-Range"))
	case resp.StatusCode == http.StatusOK:
		// We asked for "Range: bytes=0-0" and got a 200 with the whole body:
		// the server demonstrably ignored the range request. A server that
		// still sends "Accept-Ranges: bytes" is lying (some CDNs and proxies
		// do), and observed behavior wins over the header — otherwise we
		// would pick ranged mode and fail every download against it.
		p.RangeOK = false
		if resp.ContentLength > 0 {
			p.Size = resp.ContentLength
		}
	default:
		p.Error = fmt.Sprintf("http %d", resp.StatusCode)
	}
	return p
}

// "bytes 0-0/12345" -> 12345
func parseContentRangeTotal(v string) int64 {
	_, total, ok := strings.Cut(v, "/")
	if !ok {
		return -1
	}
	total = strings.TrimSpace(total)
	if total == "*" {
		return -1
	}
	n, err := strconv.ParseInt(total, 10, 64)
	if err != nil {
		return -1
	}
	return n
}
