package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"opendrain/internal/backend"
	"opendrain/internal/core"
)

//go:embed skills/opendrain/SKILL.md
var skillMD []byte

//go:embed skills/fetch/SKILL.md
var legacySkillMD []byte

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "fetch":
		runFetch(os.Args[2:])
	case "status":
		runStatus(os.Args[2:])
	case "clean":
		runClean(os.Args[2:])
	case "manifest":
		runManifest(os.Args[2:])
	case "verify":
		runVerify(os.Args[2:])
	case "read":
		runRead(os.Args[2:])
	case "install":
		runInstall()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `opendrain — agent-native download layer

Usage:
  opendrain fetch --url URL --target PATH [--checksum sha256:HEX]
                  [--mirror URL]... [--no-resume] [--backend auto|aria2|native]
  opendrain status (--url URL --target PATH | --id ID)
  opendrain clean (--url URL --target PATH | --id ID | --all)
  opendrain manifest [--verify]      list every file this agent manages
  opendrain verify [--target PATH]   re-hash managed files against stored digests
  opendrain read --target PATH [--grep RE] [--context N] [--head N | --tail N]
                          [--max-bytes N]   bounded excerpt, no bulk context cost
  opendrain install          install the opendrain skill into agent configs

fetch(url, target, checksum?, mirrors?, resume?) — bytes go to disk, one JSON
result line to stdout, errors are structured and always leave the target path
free of partial files. Downloads are resumable across processes and sessions.
`)
}

type strList []string

func (s *strList) String() string     { return strings.Join(*s, ",") }
func (s *strList) Set(v string) error { *s = append(*s, v); return nil }

func emit(v any) {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal:", err)
		os.Exit(1)
	}
	fmt.Println(string(b))
}

func runFetch(args []string) {
	fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var mirrors strList
	urlF := fs.String("url", "", "source URL (http/https)")
	targetF := fs.String("target", "", "local destination path")
	checksumF := fs.String("checksum", "", `expected digest, e.g. sha256:<64 hex chars>`)
	fs.Var(&mirrors, "mirror", "candidate mirror URL (repeatable)")
	fs.Var(&mirrors, "mirrors", "alias of --mirror (repeatable)")
	noResume := fs.Bool("no-resume", false, "restart from scratch, ignoring previous state")
	backendF := fs.String("backend", "auto", "auto | aria2 | native")
	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}
	if *urlF == "" || *targetF == "" {
		usage()
		os.Exit(2)
	}
	if len(fs.Args()) > 0 {
		fmt.Fprintf(os.Stderr, "unexpected positional args: %v\n", fs.Args())
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := core.NewStore()
	if err != nil {
		emit(&core.Result{OK: false, Status: core.StatusFailed,
			Error: &core.ErrInfo{Code: core.CodeInternal, Message: err.Error()}})
		os.Exit(1)
	}

	// Progress ticker to stderr (humans only; agents read stdout JSON).
	id := core.IDFor(*urlF, *targetF)
	tty := isTTY(os.Stderr)
	done := make(chan struct{})
	if tty {
		go func() {
			t := time.NewTicker(time.Second)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-ctx.Done():
					return
				case <-t.C:
					if st, err := store.Get(id); err == nil && st != nil && st.Total > 0 {
						fmt.Fprintf(os.Stderr, "[opendrain] %s / %s (%.0f%%) via %s\n",
							core.FormatBytes(st.Done), core.FormatBytes(st.Total),
							100*float64(st.Done)/float64(st.Total), st.Selected)
					}
				}
			}
		}()
	}

	req := core.Request{
		URL:      *urlF,
		Target:   *targetF,
		Checksum: *checksumF,
		Mirrors:  mirrors,
		Resume:   !*noResume,
		Backend:  *backendF,
	}
	backends := []core.Downloader{backend.NewAria2(), backend.NewNative()}
	res, _ := core.Fetch(ctx, req, store, backends)
	close(done)
	emit(res)
	if !res.OK {
		os.Exit(1)
	}
}

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func runStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	urlF := fs.String("url", "", "")
	targetF := fs.String("target", "", "")
	idF := fs.String("id", "", "")
	_ = fs.Parse(args)
	var id string
	switch {
	case *idF != "":
		id = *idF
	case *urlF != "" && *targetF != "":
		id = core.IDFor(*urlF, *targetF)
	default:
		emit(&core.Result{OK: false, Status: core.StatusFailed,
			Error: &core.ErrInfo{Code: core.CodeBadInput, Message: "status needs --url --target or --id"}})
		os.Exit(2)
	}
	store, err := core.NewStore()
	if err != nil {
		os.Exit(1)
	}
	st, err := store.Get(id)
	if err != nil {
		if errors.Is(err, core.ErrCorruptState) {
			// Distinguish "never existed" from "exists but is broken" — the
			// latter needs a different recovery step from the caller.
			emit(&core.Result{OK: false, Status: core.StatusFailed, ID: id,
				Warnings: []string{
					"state file is corrupt; run `opendrain clean --id " + id + "` to drop it, " +
						"or simply re-run the same fetch command — it self-heals by starting over",
				},
				Error: &core.ErrInfo{Code: core.CodeStateCorrupt, Message: err.Error()}})
			os.Exit(1)
		}
		emit(&core.Result{OK: false, Status: core.StatusFailed, ID: id,
			Error: &core.ErrInfo{Code: core.CodeInternal, Message: "read state: " + err.Error()}})
		os.Exit(1)
	}
	if st == nil {
		emit(&core.Result{OK: false, Status: core.StatusFailed, ID: id,
			Error: &core.ErrInfo{Code: core.CodeNotFound, Message: "no download state found"}})
		os.Exit(1)
	}
	emit(map[string]any{"ok": true, "state": st})
}

// runClean drops download state so the next fetch starts from scratch. It is
// the escape hatch for a task whose state file is unusable.
func runClean(args []string) {
	fs := flag.NewFlagSet("clean", flag.ExitOnError)
	urlF := fs.String("url", "", "")
	targetF := fs.String("target", "", "")
	idF := fs.String("id", "", "")
	allF := fs.Bool("all", false, "remove every state file")
	_ = fs.Parse(args)

	store, err := core.NewStore()
	if err != nil {
		emit(&core.Result{OK: false, Status: core.StatusFailed,
			Error: &core.ErrInfo{Code: core.CodeInternal, Message: err.Error()}})
		os.Exit(1)
	}

	var ids []string
	switch {
	case *allF:
		entries, rerr := os.ReadDir(store.Dir())
		if rerr != nil {
			emit(&core.Result{OK: false, Status: core.StatusFailed,
				Error: &core.ErrInfo{Code: core.CodeInternal, Message: "read state dir: " + rerr.Error()}})
			os.Exit(1)
		}
		for _, e := range entries {
			name := e.Name()
			switch {
			case strings.HasSuffix(name, ".json"):
				ids = append(ids, strings.TrimSuffix(name, ".json"))
			case strings.HasSuffix(name, ".json.corrupt"):
				ids = append(ids, strings.TrimSuffix(name, ".json.corrupt"))
			}
		}
	case *idF != "":
		ids = append(ids, *idF)
	case *urlF != "" && *targetF != "":
		ids = append(ids, core.IDFor(*urlF, *targetF))
	default:
		emit(&core.Result{OK: false, Status: core.StatusFailed,
			Error: &core.ErrInfo{Code: core.CodeBadInput, Message: "clean needs --id, --url with --target, or --all"}})
		os.Exit(2)
	}

	removed, missing := []string{}, []string{}
	for _, id := range ids {
		p := store.Path(id)
		if err := os.Remove(p); err != nil {
			missing = append(missing, p)
			continue
		}
		removed = append(removed, p)
	}
	emit(map[string]any{
		"ok":        true,
		"removed":   removed,
		"not_found": missing,
		"note":      "state dropped; the next fetch starts from scratch (partial .part files are overwritten, not deleted)",
	})
}

// runManifest is the session-start call: an agent sees every file previous
// sessions (or itself) put on disk, with integrity state, in one JSON object.
func runManifest(args []string) {
	fs := flag.NewFlagSet("manifest", flag.ExitOnError)
	verifyF := fs.Bool("verify", false, "re-hash every on-disk file against its recorded digest")
	_ = fs.Parse(args)
	begin := time.Now()
	store, err := core.NewStore()
	if err != nil {
		emit(&core.Result{OK: false, Status: core.StatusFailed,
			Error: &core.ErrInfo{Code: core.CodeInternal, Message: err.Error()}})
		os.Exit(1)
	}
	m, err := store.Manifest(*verifyF, begin)
	if err != nil {
		emit(&core.Result{OK: false, Status: core.StatusFailed,
			Error: &core.ErrInfo{Code: core.CodeInternal, Message: err.Error()}})
		os.Exit(1)
	}
	emit(m)
}

// runVerify re-hashes files and reports which ones still match the digest
// OpenDrain recorded when it published them. Target filter is optional.
func runVerify(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	targetF := fs.String("target", "", "only verify this target path")
	_ = fs.Parse(args)
	begin := time.Now()
	store, err := core.NewStore()
	if err != nil {
		emit(&core.Result{OK: false, Status: core.StatusFailed,
			Error: &core.ErrInfo{Code: core.CodeInternal, Message: err.Error()}})
		os.Exit(1)
	}
	m, err := store.Manifest(true, begin)
	if err != nil {
		emit(&core.Result{OK: false, Status: core.StatusFailed,
			Error: &core.ErrInfo{Code: core.CodeInternal, Message: err.Error()}})
		os.Exit(1)
	}
	if *targetF != "" {
		abs, aerr := filepath.Abs(*targetF)
		if aerr != nil {
			emit(&core.Result{OK: false, Status: core.StatusFailed,
				Error: &core.ErrInfo{Code: core.CodeBadInput, Message: aerr.Error()}})
			os.Exit(2)
		}
		filtered := []core.ManifestEntry{}
		for _, e := range m.Entries {
			if e.Target == abs {
				filtered = append(filtered, e)
			}
		}
		m.Entries = filtered
		m.Count = len(filtered)
		m.OnDisk = 0
		for _, e := range filtered {
			if e.OnDisk {
				m.OnDisk++
			}
		}
	}
	bad := 0
	for _, e := range m.Entries {
		if e.Verified != nil && !*e.Verified {
			bad++
		}
	}
	emit(map[string]any{"ok": bad == 0, "checked": m.Count, "on_disk": m.OnDisk,
		"mismatches": bad, "entries": m.Entries, "duration_ms": m.DurationMS})
	if bad > 0 {
		os.Exit(1)
	}
}

// runRead emits a bounded excerpt to stdout as plain text (not JSON) so the
// agent can pipe it into grep/python or read just the lines it needs.
func runRead(args []string) {
	fs := flag.NewFlagSet("read", flag.ExitOnError)
	targetF := fs.String("target", "", "local file to read")
	patternF := fs.String("grep", "", "emit lines matching this regex")
	contextF := fs.Int("context", 0, "lines before/after each match")
	headF := fs.Int("head", 0, "first N lines")
	tailF := fs.Int("tail", 0, "last N lines")
	maxF := fs.Int64("max-bytes", core.DefaultReadMax, "hard cap on output bytes")
	_ = fs.Parse(args)
	if *targetF == "" {
		fmt.Fprintln(os.Stderr, "read needs --target PATH")
		os.Exit(2)
	}
	text, err := core.ReadFileSlice(*targetF, core.ReadOptions{
		Pattern: *patternF, Context: *contextF, Head: *headF,
		Tail: *tailF, MaxBytes: *maxF,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "read:", err)
		os.Exit(1)
	}
	fmt.Print(text)
}

func runInstall() {
	home, err := os.UserHomeDir()
	if err != nil {
		os.Exit(1)
	}
	// Primary skill is installed everywhere we detect, plus the legacy alias
	// so agents that already learned `fetch` keep working after an upgrade.
	agents := []struct {
		base  string
		label string
		force bool
	}{
		{filepath.Join(home, ".config", "opencode", "skills"), "opencode (global)", true},
		{filepath.Join(home, ".workbuddy", "skills"), "workbuddy", dirExists(filepath.Join(home, ".workbuddy"))},
		{filepath.Join(home, ".claude", "skills"), "claude code", dirExists(filepath.Join(home, ".claude"))},
		{filepath.Join(home, ".codex", "skills"), "codex", dirExists(filepath.Join(home, ".codex"))},
		{filepath.Join(home, ".zcode", "skills"), "zcode", dirExists(filepath.Join(home, ".zcode"))},
		{filepath.Join(home, ".agents", "skills"), "agents", dirExists(filepath.Join(home, ".agents"))},
	}
	type target struct {
		dir    string
		label  string
		force  bool
		legacy bool
	}
	targets := []target{}
	for _, a := range agents {
		targets = append(targets,
			target{filepath.Join(a.base, "opendrain"), a.label, a.force, false},
			target{filepath.Join(a.base, "fetch"), a.label + " (legacy fetch)", a.force, true},
		)
	}
	type installed struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
	}
	var report []installed
	for _, t := range targets {
		if !t.force && !dirExists(t.dir) {
			continue
		}
		if err := os.MkdirAll(t.dir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", t.label, err)
			continue
		}
		dst := filepath.Join(t.dir, "SKILL.md")
		content := skillMD
		if t.legacy {
			// Never overwrite an agent's own customized fetch skill.
			if _, err := os.Stat(dst); err == nil {
				continue
			}
			content = legacySkillMD
		}
		if err := os.WriteFile(dst, content, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "skip %s: %v\n", t.label, err)
			continue
		}
		report = append(report, installed{Path: dst, Kind: "skill:" + t.label})
	}

	// Make sure `opendrain` itself is reachable on PATH.
	if p, err := exec.LookPath("opendrain"); err == nil {
		fmt.Fprintf(os.Stderr, "binary already on PATH: %s\n", p)
	} else if self, err := os.Executable(); err == nil {
		binDir := filepath.Join(home, ".local", "bin")
		if err := os.MkdirAll(binDir, 0o755); err == nil {
			dst := filepath.Join(binDir, "opendrain")
			if err := copyFile(self, dst, 0o755); err == nil {
				fmt.Fprintf(os.Stderr, "binary copied to %s (make sure this dir is on PATH)\n", dst)
			}
		}
	}
	emit(map[string]any{"ok": true, "installed": report})
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
