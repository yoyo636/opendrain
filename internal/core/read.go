package core

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
)

// ReadOptions controls a bounded, on-demand slice of a local file. The whole
// point is that content never enters the agent's context in bulk: fetch puts
// bytes on disk, read returns only the lines the agent actually needs.
type ReadOptions struct {
	Pattern  string // regex; when set, emit matching lines (with --context)
	Context  int    // lines before/after each match
	Head     int    // first N lines
	Tail     int    // last N lines
	MaxBytes int64  // hard cap on returned text (default 256KB)
}

const DefaultReadMax = 256 << 10

// ReadFileSlice returns a bounded textual excerpt of path. Exactly one mode
// applies: grep (Pattern) > head > tail > whole-file (bounded by MaxBytes).
func ReadFileSlice(path string, opt ReadOptions) (string, error) {
	if opt.MaxBytes <= 0 {
		opt.MaxBytes = DefaultReadMax
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if opt.Pattern != "" {
		re, err := regexp.Compile(opt.Pattern)
		if err != nil {
			return "", fmt.Errorf("bad pattern: %w", err)
		}
		return grepExcerpt(f, re, opt)
	}
	if opt.Head > 0 {
		return lineExcerpt(f, opt.Head, false, opt.MaxBytes)
	}
	if opt.Tail > 0 {
		return lineExcerpt(f, opt.Tail, true, opt.MaxBytes)
	}
	return wholeExcerpt(f, opt.MaxBytes)
}

func appendBounded(sb *strings.Builder, limit int64, parts ...string) {
	for _, p := range parts {
		if int64(sb.Len())+int64(len(p)) > limit {
			room := int(limit) - sb.Len()
			if room > 0 {
				sb.WriteString(p[:room])
			}
			return
		}
		sb.WriteString(p)
	}
}

func wholeExcerpt(f *os.File, limit int64) (string, error) {
	var sb strings.Builder
	buf := make([]byte, 32<<10)
	for int64(sb.Len()) < limit {
		n, err := f.Read(buf)
		if n > 0 {
			appendBounded(&sb, limit, string(buf[:n]))
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return sb.String(), err
		}
	}
	return sb.String(), nil
}

func lineExcerpt(f *os.File, n int, tail bool, limit int64) (string, error) {
	var sb strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	if tail {
		ring := make([]string, 0, n)
		for sc.Scan() {
			ring = append(ring, sc.Text()+"\n")
			if len(ring) > n {
				ring = ring[1:]
			}
		}
		for _, line := range ring {
			appendBounded(&sb, limit, line)
		}
	} else {
		count := 0
		for sc.Scan() && count < n {
			appendBounded(&sb, limit, sc.Text(), "\n")
			count++
		}
	}
	return sb.String(), sc.Err()
}

// grepExcerpt does a single streaming pass: ring holds the preceding context
// lines, pending counts the following lines still to emit after a match.
func grepExcerpt(f *os.File, re *regexp.Regexp, opt ReadOptions) (string, error) {
	var sb strings.Builder
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	ring := []string{}
	pending := 0
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		matched := re.MatchString(line)
		if matched {
			for _, prev := range ring {
				appendBounded(&sb, opt.MaxBytes, prev, "\n")
			}
			ring = ring[:0]
			appendBounded(&sb, opt.MaxBytes, fmt.Sprintf("%d: %s\n", lineNo, line))
			pending = opt.Context
		} else if pending > 0 {
			appendBounded(&sb, opt.MaxBytes, fmt.Sprintf("%d: %s\n", lineNo, line))
			pending--
		} else if opt.Context > 0 {
			ring = append(ring, fmt.Sprintf("%d: %s", lineNo, line))
			if len(ring) > opt.Context {
				ring = ring[1:]
			}
		}
		if int64(sb.Len()) >= opt.MaxBytes {
			break
		}
	}
	return sb.String(), sc.Err()
}
