package core

import (
	"fmt"
	"time"
)

// Request mirrors the fetch(url, target, checksum?, mirrors?, resume?) call surface.
type Request struct {
	URL      string   `json:"url"`
	Mirrors  []string `json:"mirrors,omitempty"`
	Target   string   `json:"target"`
	Checksum string   `json:"checksum,omitempty"` // "sha256:<hex>"
	Resume   bool     `json:"resume"`
	Backend  string   `json:"backend,omitempty"` // auto | aria2 | native
}

// Chunk tracks one byte range of the target file.
type Chunk struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"` // inclusive; -1 means open-ended (unknown size)
	Done  int64 `json:"done"`
}

func (c Chunk) Size() int64 {
	if c.End < 0 {
		return -1
	}
	return c.End - c.Start + 1
}

// ProbeInfo is the result of one mirror probe.
type ProbeInfo struct {
	URL       string `json:"url"`
	LatencyMS int64  `json:"latency_ms"`
	RangeOK   bool   `json:"range"`
	Size      int64  `json:"size"` // -1 when unknown
	Status    int    `json:"status"`
	Error     string `json:"error,omitempty"`
}

// ErrInfo is the structured error every failure carries.
type ErrInfo struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Detail  map[string]any `json:"detail,omitempty"`
}

func (e *ErrInfo) Error() string { return e.Message }

// Error wraps ErrInfo so backends return structured failures through plain error values.
type Error struct{ Info ErrInfo }

func (e *Error) Error() string { return e.Info.Message }

func Fail(code, msg string, detail map[string]any) *Error {
	return &Error{Info: ErrInfo{Code: code, Message: msg, Detail: detail}}
}

// State is the on-disk, crash-safe download state. Everything needed to
// resume after the process dies lives here, never in memory alone.
type State struct {
	ID        string      `json:"id"`
	Request   Request     `json:"request"`
	Backend   string      `json:"backend"`
	Status    string      `json:"status"`
	Mode      string      `json:"mode,omitempty"` // ranged | single
	PartPath  string      `json:"part_path,omitempty"`
	Total     int64       `json:"total_bytes"` // 0 when unknown
	Done      int64       `json:"done_bytes"`
	Chunks    []Chunk     `json:"chunks,omitempty"`
	Sha256    string      `json:"sha256,omitempty"`
	Probes    []ProbeInfo `json:"mirrors_probed,omitempty"`
	Selected  string      `json:"selected_mirror,omitempty"`
	Error     *ErrInfo    `json:"error,omitempty"`
	Warnings  []string    `json:"warnings,omitempty"`
	Attempts  int         `json:"attempts,omitempty"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
}

const (
	StatusNew         = "new"
	StatusProbing     = "probing"
	StatusDownloading = "downloading"
	StatusVerifying   = "verifying"
	StatusCompleted   = "completed"
	StatusFailed      = "failed"
	StatusCancelled   = "cancelled"
)

// Error codes returned in {"error":{"code":...}}.
const (
	CodeNoMirror     = "no_mirror_reachable"
	CodeAllFailed    = "all_mirrors_failed"
	CodeChecksum     = "checksum_mismatch"
	CodeHTTP         = "http_error"
	CodeBackend      = "backend_failed"
	CodeCancelled    = "cancelled"
	CodeBadInput     = "bad_request"
	CodeInternal     = "internal_error"
	CodeNotFound     = "state_not_found"
	CodeIncomplete   = "incomplete"
	CodeStateCorrupt = "state_corrupt"
)

const (
	ModeRanged = "ranged"
	ModeSingle = "single"
)

// MakeChunks splits [0, total) into n byte ranges; the last chunk absorbs
// the remainder.
func MakeChunks(total int64, n int) []Chunk {
	if n < 1 {
		n = 1
	}
	base := total / int64(n)
	var out []Chunk
	var off int64
	for i := 0; i < n && off < total; i++ {
		end := off + base - 1
		if i == n-1 || end >= total {
			end = total - 1
		}
		out = append(out, Chunk{Start: off, End: end})
		off = end + 1
	}
	return out
}

// CandidateURLs returns probe results sorted best-first (ProbeAll already
// ordered them), skipping mirrors that failed to respond.
func CandidateURLs(st *State) []string {
	var out []string
	for _, p := range st.Probes {
		if p.Error == "" {
			out = append(out, p.URL)
		}
	}
	return out
}

func FormatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGTPE"[exp])
}
