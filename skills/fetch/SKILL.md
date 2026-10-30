---
name: fetch
description: Agent-native file download to disk. Use whenever you need to download ANY file to local storage - datasets, model weights, archives, wheels, binaries, PDFs, release assets, large HTML docs - especially when the file is over ~100KB, is binary, needs integrity verification (checksum), has candidate mirrors, or the download might be interrupted. Replaces curl/wget downloads: state persists on disk, so re-calling the same command after any crash or session boundary resumes instead of restarting, and the target path never contains a partial file. Do NOT use it to read a page's text into the conversation (use webfetch for that), for authenticated URLs (use curl with credentials), or for git repositories (use git clone).
---

# OpenDrain fetch

Download bytes to disk with zero content-token cost: the tool writes the file
to the filesystem and returns one small JSON line on stdout. Afterwards, read
the file selectively (grep / sed / python) instead of pulling it through your
context window.

## When to use vs alternatives

| Situation | Use |
|---|---|
| Any file must land on disk for later processing | `fetch` |
| File > ~100KB, or any binary (weights, archives, wheels) | `fetch` |
| Download is long, may be interrupted, may cross sessions | `fetch` (resumable) |
| Multiple candidate mirrors exist | `fetch` (probes + auto-failover) |
| You only need page text summarized into the conversation | webfetch |
| URL needs cookies / API tokens | `curl` with credentials |
| Git repositories | `git clone` |

## Calling convention

`fetch(url, target, checksum?, mirrors?, resume?)` maps to:

```sh
opendrain fetch --url URL --target PATH \
  [--checksum sha256:HEX] [--mirror URL]... [--no-resume] [--backend auto|aria2|native]
```

- `url` (required): http(s) source.
- `target` (required): local path to write. While downloading, bytes live in
  `<target>.part`; the real path appears only when the file is complete and
  verified, so a partial file can never be mistaken for the real one.
- `checksum` (optional): `sha256:<hex>`. On mismatch the partial file is
  deleted and the call fails with `checksum_mismatch` - nothing bad is left on
  disk. ALWAYS pass this when the source publishes a checksum.
- `mirrors` (optional): repeat `--mirror URL` per candidate. Before starting,
  every candidate gets one cheap probe (time-to-first-byte + Range support);
  the best one is chosen, and mid-download failures automatically fall over to
  the next mirror, keeping already-downloaded chunks. Mirrors must serve
  identical bytes; the checksum protects you when they don't.
- `resume` (optional, default true): after ANY interruption (kill, compaction,
  Ctrl-C, network loss), just re-call the same command - completed chunks are
  kept and the download continues. `--no-resume` forces a clean restart.

## Result contract

Success (exit 0), one JSON object:

```json
{"ok": true, "status": "completed", "target": "/path/file", "bytes": 1234567,
 "sha256": "sha256:...", "backend": "aria2", "selected_mirror": "https://...",
 "mirrors": [...], "resumed": false, "duration_ms": 431}
```

`sha256` is the digest of the bytes actually written. When you did not pass a
checksum, decide yourself whether to trust the file (e.g. compare against the
project's published digest) before acting on it.

`resumed: true` means bytes already on disk were reused. It is only reported
when the partial file was actually there — if the partial file was lost, the
download restarts from zero and `resumed` stays false, so never assume a
`resumed: true` you did not see.

`warnings` (optional) lists anything the tool had to repair on the way, e.g.
`"state file was corrupt and has been reset"`.

Failure (exit 1): `{"ok": false, "status": "failed|cancelled", "error":
{"code": ..., "message": ...}}`. Codes: `no_mirror_reachable`,
`all_mirrors_failed`, `checksum_mismatch`, `http_error`, `backend_failed`,
`cancelled`, `bad_request`, `internal_error`, `incomplete`, `state_not_found`,
`state_corrupt`.
Cancellation keeps partial state on disk - re-calling resumes.

State lives in `~/.opendrain/state/<hash>.json` (hash of url+target; relative
and absolute target paths map to the same hash). Delete it or pass
`--no-resume` to start over. `opendrain status --url URL --target PATH`
inspects a running/interrupted download without touching it. `opendrain clean
(--url URL --target PATH | --id ID | --all)` drops download state so the next
fetch starts from scratch - use it when `status` reports `state_corrupt`
(a corrupt state file never blocks a download: `fetch` self-heals by starting
over).

## Examples

Cache a large docs page, then grep only the section you need:

```sh
opendrain fetch --url https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers \
  --target ~/.cache/mdn/http-headers.html
grep -A 3 '<code>Accept-Ranges</code>' ~/.cache/mdn/http-headers.html
```

Model weights verified against the project's published digest:

```sh
opendrain fetch --url https://example.org/releases/model.safetensors \
  --target /tmp/model.safetensors --checksum sha256:9f86d0818...
```

With mirrors (fastest wins, automatic failover):

```sh
opendrain fetch --url https://mirror1.example.org/big.tar.gz \
  --mirror https://mirror2.example.org/big.tar.gz \
  --target /tmp/big.tar.gz --checksum sha256:9f86d0818...
```

## Notes

- Prefers aria2c (mature multi-connection downloader) when installed; falls
  back to a built-in Go downloader otherwise. Same CLI, same semantics.
- Default behavior is tuned for agents: zero config, structured errors,
  atomic publish, resume by default.
