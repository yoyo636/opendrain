# DAP v1 — Download Action Protocol

**Status:** draft v1 · **Author:** OpenDrain project · **License:** MIT

DAP is a transport-agnostic protocol between an **Agent runtime** (the client)
and a **Download Action Layer** (the server). The agent expresses a download
*intent*; the layer owns state, strategy, verification and recovery, and
returns a compact *handle*. Content never crosses the protocol boundary.

---

## 1. Scope

### In scope
- Downloading an http(s) URL to a local file
- Resumable state, mirror selection, integrity verification, atomic publish
- Session-level file inventory ("what do I already have")
- Capability discovery and structured errors

### Out of scope
- Content interpretation (summarization, parsing, RAG)
- Executing files, generic shell, writing source code
- Credential management beyond per-request source authentication headers
- Non-download actions (browser automation, messaging, compute)

A conforming implementation MUST support every `MUST` item in this document.
`SHOULD` items are recommended; `MAY` items are optional.

---

## 2. Roles and model

```
Agent runtime ──request──> DAL server ──> bytes on disk
              <─response──           (state + strategy live here)
```

- **Client**: any agent runtime (CLI agent, IDE extension, web agent, etc.).
- **Server**: any DAL implementation (OpenDrain is a reference server).
- **Session**: one or more client processes sharing the same state store. DAP
  has no notion of a persistent connection; every request is self-contained.

The server is **stateful by design**. The client is stateless: it may be killed
and restarted at any time, and re-issuing an equivalent request must recover.

---

## 3. Wire format

The canonical encoding is **JSON**. A transport carries exactly one JSON
Request or Response object per message. DAP does not require a specific
transport; bindings define framing.

### 3.1 Request envelope

```json
{
  "dap": "1.0",
  "id": "req-42",
  "method": "act",
  "params": { }
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `dap` | string | MUST | Protocol version, `"1.0"` |
| `id` | string | MUST | Client-generated correlation id; echoed in response |
| `method` | string | MUST | One of §5 |
| `params` | object | MUST (may be `{}`) | Method parameters |

### 3.2 Response envelope

```json
{
  "dap": "1.0",
  "id": "req-42",
  "result": { },
  "error": null
}
```

Exactly one of `result` / `error` is non-null. On success `result` is the
method's result object. On failure `error` is an Error object (§4.4).

### 3.3 Standard bindings

| Binding | Framing | Use |
|---|---|---|
| **stdio** | one JSON object per line (NDJSON), UTF-8 | local agent processes; REQUIRED for conformance |
| **exec** | one request via argv `--request <json>`, one response on stdout | one-shot CLI adapters; MAY |
| **http** | `POST /dap` with JSON body | remote DAL; MAY |

A server MUST implement the stdio binding. Other bindings are optional but
must preserve identical envelope semantics.

---

## 4. Data types

### 4.1 Intent (params of `act`)

| Field | Type | Required | Default | Meaning |
|---|---|---|---|---|
| `url` | string | MUST | — | http(s) source URL |
| `target` | string | MUST | — | local destination path; normalized to absolute by server |
| `checksum` | string | MAY | absent | `sha256:<64 hex>`; enables strong verification |
| `mirrors` | string[] | MAY | `[]` | candidate mirror URLs |
| `task_key` | string | MAY | absent | stable identity when the URL rotates |
| `backend` | enum `auto\|native\|aria2` | MAY | `auto` | execution backend preference |
| `resume` | boolean | MAY | `true` | whether existing progress may be reused |

Unknown params MUST be ignored (forward compatibility).

### 4.2 Handle (result of `act`)

| Field | Type | Meaning |
|---|---|---|
| `id` | string | server's task identity (sha256 of normalized intent identity) |
| `target` | string | absolute destination path |
| `bytes` | integer | byte count of the published file |
| `sha256` | string | `sha256:<hex>` of bytes actually written |
| `integrity` | enum `checksum\|unverified` | `checksum` = matched caller digest; `unverified` = transfer/size only |
| `resumed` | boolean | true only if pre-existing bytes were actually reused |
| `cached` | boolean | true if the call hit the completed-file fast path |
| `status` | enum | `completed\|failed\|cancelled` |
| `warnings` | string[] | non-fatal notices (e.g. orphan part discarded) |
| `error` | Error? | present on failure |

A handle MUST NOT contain file content. A handle with `status: completed` and
`error: null` guarantees the target path holds a complete file; it does NOT
guarantee source correctness unless `integrity: checksum`.

### 4.3 ManifestEntry / Manifest

`manifest` returns:

```json
{
  "ok": true,
  "count": 1,
  "on_disk": 1,
  "entries": [
    {
      "id": "…", "url": "…", "target": "…",
      "status": "completed", "bytes": 123, "sha256": "sha256:…",
      "on_disk": true, "verified": true, "note": ""
    }
  ],
  "duration_ms": 3
}
```

Entries are sorted by most recent update first. `verified` is present only
when `verify: true` was requested and the file exists on disk.

### 4.4 Error

```json
{
  "code": "checksum_mismatch",
  "message": "…",
  "detail": { "expected": "sha256:…", "actual": "sha256:…" }
}
```

Standard codes (a server MAY add others, but MUST document them):

| Code | Meaning |
|---|---|
| `bad_request` | malformed intent / unsupported method / bad version |
| `no_mirror_reachable` | no candidate responded |
| `all_mirrors_failed` | transfer failed on every mirror after retries |
| `checksum_mismatch` | digest did not match expected |
| `incomplete` | published size disagrees with recorded size |
| `state_corrupt` | persisted state exists but cannot be parsed |
| `state_not_found` | no task for the supplied identity |
| `cancelled` | request cancelled; partial state retained |
| `backend_failed` | backend could not start / RPC failed |
| `internal_error` | unexpected server failure |
| `unsupported_method` | method not implemented |
| `unsupported_version` | `dap` version not supported |

---

## 5. Methods

### 5.1 `capabilities` (SHOULD)

Params: `{}`. Result:

```json
{
  "dap": "1.0",
  "methods": ["capabilities", "act", "manifest", "verify", "clean", "read"],
  "backends": ["native", "aria2"],
  "features": ["resume", "mirrors", "checksum", "task_key", "integrity_flag"]
}
```

Clients SHOULD call this once; servers MUST answer `capabilities` if they
advertise it in `methods`.

### 5.2 `act` (MUST)

Params: Intent (§4.1). Result: Handle (§4.2).

Semantics:
1. Normalize target to absolute path; derive task id from `task_key` if
   present, otherwise canonical URL + absolute target.
2. If a completed file exists at the target, re-hash it. If its size differs
   from the recorded size, quarantine it and re-download in this call.
3. If this is a fresh task, remove orphan `<target>.part` / `.part.aria2`.
4. Probe mirrors (reusing a fresh probe set for the same URL set), download
   into `<target>.part`, verify checksum and size, atomically publish.
5. Return a handle. On failure return an Error; never leave a partial file at
   the target path.

**Idempotence:** re-issuing the same `act` request concurrently or after a
crash MUST NOT corrupt state or produce duplicate downloads. The server MUST
serialize per task id across processes (advisory lock or equivalent).

### 5.3 `manifest` (SHOULD)

Params: `{ "verify": boolean }` (default `false`). Result: Manifest (§4.3).

With `verify: true`, the server re-hashes every on-disk managed file. This is
expensive; clients SHOULD use `verify` (§5.4) for a single file instead.

### 5.4 `verify` (SHOULD)

Params: `{ "target": string }`. Result: ManifestEntry with `verified` set.
The server MUST hash only the named file, not the whole store.

### 5.5 `clean` (MAY)

Params: one of `{ "id" }`, `{ "url", "target" }`, or `{ "all": true }`.
Result: `{ "removed": string[], "not_found": string[], "busy": string[], "note": string }`.
The server MUST NOT delete state or partial files for a task currently being
acted upon; it MUST report it in `busy` instead.

### 5.6 `read` (MAY)

Params: `{ "target", "grep"?, "context"?, "head"?, "tail"?, "max_bytes"? }`.
Result: `{ "text": string, "truncated": boolean }`. Output MUST be capped by
`max_bytes` (server default 256 KiB). This method is bounded content access —
the one intentional exception to "no content crosses the boundary", and it is
strictly limited.

---

## 6. State model

Server-side state is opaque to clients; there is no DAP method to set raw
state. The logical lifecycle is:

```
new -> probing -> downloading -> verifying -> completed
                       |              |
                       +-> failed / cancelled
```

- `failed` / `cancelled` retain partial state and are resumable by re-issuing
  `act` with the same identity.
- Identity is stable across process restarts and (via `task_key`) across URL
  rotation.
- State storage MUST be crash-safe (atomic write; fsync on final commit).
- A corrupt state file MUST NOT permanently block a task: the server resets
  and reports it in `warnings` or as `state_corrupt`.

---

## 7. Identity and signature rotation

Task identity is derived in this order:
1. `task_key` (caller-supplied stable key) + normalized absolute target;
2. otherwise canonical URL + normalized absolute target.

Canonicalization MUST strip rotating signature parameters (e.g. `sig`,
`se`, `Expires`, `Signature`, `X-Amz-*`, `X-Goog-*`, `Policy`,
`Key-Pair-Id`, `response-content-*`) for hosts known to rotate them, while
preserving parameters that select bytes (e.g. `id`, `ref`, `download`).
Unknown hosts MUST be left unchanged. Clients with unusual sources SHOULD
supply `task_key`.

---

## 8. Security requirements

- Servers MUST cap redirects (recommended 10) and refuse https→http downgrade.
- Credentials, if supported, MUST NOT be persisted in state and MUST be
  stripped on redirects to non-origin hosts.
- Servers MUST NOT execute downloaded files.
- `target` paths MUST be normalized; identity must not be derivable from a
  path outside the intended target (e.g. `../` traversal in `--id`).
- State files SHOULD be private (`0700` dir / `0600` file on POSIX).
- `read` MUST enforce `max_bytes`; a client must not be able to dump an
  unbounded file into its context.

---

## 9. Conformance

An implementation conforms to DAP v1 if it:
- implements the stdio NDJSON binding, `act`, and the error model;
- honours intent normalization, idempotence and handle semantics;
- never returns content from `act`;
- passes the protocol conformance suite (`internal/dap/dap_test.go` in the
  reference implementation; tests cover capabilities, act, bad intents,
  version/method errors, verify/manifest, envelope codec, and a client over
  an in-memory pipe).

The reference implementation is OpenDrain's `internal/dap` package (dispatcher,
NDJSON stdio binding, reference client) and the `opendrain dap` command. The
machine-readable envelope schema is `protocol/dap-v1.schema.json`.

---

## 10. Versioning

`dap` is a semantic version string. A server that does not support the
requested major version MUST return `unsupported_version` with detail listing
supported versions. Additive methods and params are allowed within a major
version; removing or changing the meaning of a field requires a new major.
