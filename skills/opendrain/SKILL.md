---
name: opendrain
description: Agent file layer - use from session start through completion to know what files exist, fetch new bytes, verify integrity, and read only the lines needed. Replaces ad-hoc curl/wget plus grep: files land on disk with resumable state and a recorded sha256, so a killed process or compaction never loses progress, and content never floods the context window. Call `opendrain manifest` at session start, `opendrain fetch` for any new file, `opendrain verify` before trusting a cached file, and `opendrain read --grep` to inspect a large file. Do NOT use it to summarize a page into the conversation (use webfetch), for authenticated URLs (use curl with credentials), or for git repositories (use git clone).
---

# OpenDrain — the agent file layer

You are not looking at a downloader. You are looking at the file I/O layer an
agent uses for an entire session:

```
session start        work                          session end / next session
   manifest   →   fetch → verify → read      →   state stays on disk, next session sees it
```

Bytes go to disk. Context gets one small JSON object. The agent reads only the
few lines it needs. A killed process or a compaction is a normal event, not a
lost download: re-calling the same command resumes.

## Step 1 — session start: what do I already have?

```sh
opendrain manifest [--verify]
```

One JSON object listing every file OpenDrain manages: `url`, `target`, `bytes`,
`sha256`, `status`, `updated_at`, `on_disk`. Call this at the beginning of a
session (or after a compaction) to recover the file state instead of asking
the user what was downloaded.

- `--verify` re-hashes each on-disk file and adds `verified: true|false`. Extra
  time (one full read per file) — use it when integrity matters, e.g. before a
  build or a data pipeline.
- `note` explains anomalies: `target missing; re-fetch to restore`, or
  `digest mismatch; re-fetch before trusting this file`.

Do not assume a `completed` state means the file is still there; check
`on_disk`.

## Step 2 — get bytes on disk

```sh
opendrain fetch --url URL --target PATH \
  [--checksum sha256:HEX] [--mirror URL]... [--task-key KEY] \
  [--no-resume] [--backend auto|native|aria2]
```

- `url` (required): http(s) source.
- `target` (required): destination. While downloading, bytes live in
  `<target>.part`; the real path appears only when the file is complete. A
  fresh task also deletes any orphan `.part`/`.aria2` at that path, so stale
  bytes from another task can never be published as this one.
- `checksum` (optional): `sha256:<hex>`. Always pass it when the source
  publishes one. On mismatch the partial file is removed and the call fails
  with `checksum_mismatch`; for an already-cached file that fails re-hashing,
  the file is renamed to `<target>.corrupt` (never silently deleted). A
  cached file whose size no longer matches the recorded size is quarantined
  and re-downloaded in the same call.
- `mirrors` (optional): repeat `--mirror URL`. Candidates are probed in
  parallel (TTFB + Range support); the best is chosen and mid-download
  failures fall over to the next, keeping chunks already on disk. Probes are
  reused for 60s on a resumed task, so a re-call does not pay the probe cost
  again.
- `resume` (default true): after ANY interruption — kill, Ctrl-C, compaction,
  network loss — re-call the same command. `resumed: true` in the result means
  bytes already on disk were actually reused; if the partial file vanished,
  the download restarts and `resumed` stays false.
- `task-key` (optional): a stable identity for sources whose signed URL
  rotates (HuggingFace/xet, S3, CloudFront). Pass the SAME key when refreshing
  the link, otherwise the new URL looks like a new task and loses progress.
  GitHub and known HF hosts are canonicalised automatically.
- `backend` (default auto): native (up to 16 workers, small work units, zero
  subprocess overhead) is the default; `aria2` is available if installed. Both
  have the same CLI and semantics.

Result on success is one JSON object:

```json
{"ok": true, "status": "completed", "target": "/path/file", "bytes": 1234567,
 "sha256": "sha256:...", "backend": "native", "selected_mirror": "https://...",
 "mirrors": [...], "resumed": false, "cached": false, "integrity": "checksum",
 "duration_ms": 431}
```

`integrity: "checksum"` means the bytes matched the checksum you supplied.
`integrity: "unverified"` means only transfer/size integrity was checked —
`sha256` is the digest of what arrived, NOT proof that the source was correct.
For anything you will build on, pass a checksum or verify against an
independent source digest.

Failure (exit 1): `{"ok": false, "status": "failed|cancelled", "error":
{"code": ..., "message": ...}}`. Codes: `no_mirror_reachable`,
`all_mirrors_failed`, `checksum_mismatch`, `http_error`, `backend_failed`,
`cancelled`, `bad_request`, `internal_error`, `incomplete`, `state_not_found`,
`state_corrupt`. `cancelled` keeps partial state — re-call to resume.

## Step 3 — verify before you trust

```sh
opendrain verify [--target PATH]
```

Re-hashes managed files and compares with the digest recorded at publish
time. Output: `{"ok": true, "checked": N, "on_disk": N, "mismatches": 0,
"entries": [...]}`. Exit 1 when any mismatch is found — treat that file as
unusable and re-fetch it.

## Step 4 — read only what you need

```sh
opendrain read --target PATH [--grep REGEX] [--context N] [--head N | --tail N]
               [--max-bytes N]
```

Prints a bounded excerpt as plain text to stdout (not JSON). This is the other
half of "content never enters context": `fetch` stores the full bytes, `read`
returns just the lines you asked for.

- `--grep REGEX`: matching lines prefixed with their line number; `--context N`
  adds N lines before and after each match. Single streaming pass.
- `--head N` / `--tail N`: first or last N lines.
- default: whole file, capped at `--max-bytes` (default 256KB).

Then use `grep -A 3 ...`, `python`, or your own tools on the file directly if
you need more. Never paste a whole large file into the conversation.

## Step 5 — inspect / drop state

```sh
opendrain status --url URL --target PATH   # or --id ID
opendrain clean (--url URL --target PATH | --id ID | --all)
```

`status` shows the full state of one task without touching it. `clean` drops
state and its `.part`/`.aria2`/`.corrupt` evidence so the next fetch starts
from scratch; a task that is currently running is reported as `busy` instead
of being deleted. Lock files are deliberately left in place (kernel-managed).
A corrupt state never blocks a download: `fetch` self-heals by quarantining it
and starting over.

State lives in `~/.opendrain/state/<hash>.json` (hash of url + absolute target;
relative and absolute paths map to the same hash). The directory is 0700 and
state files are 0600 because they can contain presigned URLs.

## GitHub / signed URLs

GitHub is a first-class source, but rotating signed URLs are wider than GitHub:

- Credentials come from `GITHUB_TOKEN` / `GH_TOKEN` or `gh auth token`; they
  are only sent to `github.com` / `api.github.com` and are stripped on the
  redirect to `release-assets.githubusercontent.com`. The `gh` lookup is lazy
  (only on a GitHub URL) and bounded by a timeout.
- Release asset links are short-lived signed redirects. Task identity ignores
  rotating signature params, so re-calling after a link refresh still resumes.
- `*.huggingface.co`, `*.hf.co`, `xethub.hf.co` and S3/GCS signature params are
  canonicalised too. If your source is not recognised, pass `--task-key`.
- Concurrency is capped at 4 workers for GitHub assets <256MB (measured CDN
  jitter made 16-way slower); >=256MB automatically use the full 16.

## When NOT to use

| Situation | Use instead |
|---|---|
| You need a page summarized into the conversation | webfetch |
| URL needs cookies / API tokens / `--header` | `curl` with credentials |
| git repositories | `git clone` |
| BT / magnet / Metalink | aria2 directly |

## Install (all agent environments)

```sh
opendrain install
```

Installs this skill as `SKILL.md` into every detected agent config:
OpenCode (`~/.config/opencode/skills/opendrain`), WorkBuddy
(`~/.workbuddy/skills/opendrain`), Claude Code
(`~/.claude/skills/opendrain`), Codex (`~/.codex/skills/opendrain`),
zcode, and `~/.agents/skills/opendrain`. The binary is copied to
`~/.local/bin` if it is not already on PATH. WorkBuddy rescans its skills
directory on next session start; no manual cache edit is needed.

## Notes

- Pure Go standard library, zero third-party dependencies, single binary for
  5 platforms.
- `manifest` is cheap (state files only); `manifest --verify` and `verify`
  read whole files, so run them when integrity matters, not in a tight loop.
- `read` is bounded by default: an agent cannot accidentally dump a 1GB file
  into its context in one call.
