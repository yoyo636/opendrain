# Agents Don't Need a Better Downloader. They Need a Download Layer.

> An agent was downloading a 3GB model when it got interrupted. It didn't crash — it was compacted. When it came back, it was standing in the same place, and the 1.9GB already on disk belonged to nobody.

That is not a hypothetical. It happened on 2026-11-27 in a real local agent session. It points at something more fundamental than "downloads are slow": **we have been giving agents tools designed for humans, whose entire interaction model rests on assumptions that do not hold for agents.**

---

## 1. Humans and agents want different things from "download"

curl, wget, and browser download managers assume a human on the other side:

- A human watches the progress bar and retries on failure.
- A human opens the file afterwards and checks that it looks right.
- A human reads a settings panel to tune concurrency and mirrors.
- A human's attention is continuous — a download usually finishes in one sitting.

Agents satisfy none of these.

| Dimension | Human | Agent |
|---|---|---|
| Lifetime | Continuous within one session | Killed at any moment (timeout, compaction, budget) |
| Content | Opens and inspects it | **Must never enter context** |
| Failure | Sees an error, decides what to do | Needs structured, machine-readable errors |
| Configuration | Can read a settings panel | Will not read one; zero config is the only option |
| Integrity | Trusts "download complete" | Must be able to re-verify before trusting |

Translate those five differences into a spec and you get something quite different from curl:

1. **State must be on disk** — progress cannot live only in memory;
2. **Results must be small** — a few bytes of metadata, not the content;
3. **Failures must be explicit** — no silent half-files that an agent will mistake for good ones;
4. **Zero configuration** — mirror selection, concurrency, backend choice are automatic;
5. **Verifiable** — every file carries a digest that can be re-checked before use.

That is not "a downloader with extra features." It is a different category of software: **the file I/O layer of an agent session.**

---

## 2. What a "download layer" actually is

A working definition:

> A download layer is the interface between an agent and the world of bytes. It turns "I need this content" into "there is a verifiable file at this path, with this digest and this state" — and hands only the latter to the agent.

The API shape is the whole point. A traditional downloader says:

```
download(url) -> content
```

A download layer says:

```
fetch(url, target, checksum?, mirrors?, resume?) -> handle{path, digest, status, warnings}
```

Content never passes through the agent's context. What the agent receives is a **handle**: where the file is, how big it is, its digest, whether it came from cache, whether a re-call actually resumed. Everything afterwards operates on that handle: `verify` re-hashes, `read` pulls a few lines, `manifest` answers "what did previous sessions leave me" after a restart.

This sounds like a small distinction. It is architectural. It turns downloading from a one-shot I/O action into a managed lifecycle:

```
session start → manifest → fetch → verify → read → state stays on disk → visible next session
```

Once you see it that way, several "good enough" approaches stop being good enough. `curl` writing straight to the target path means incomplete bytes can appear at that path, with no way for the agent to tell. `webfetch`-style summarization compresses content into a lossy projection sent into context; the underlying bytes are never stored, and asking a second question means reprocessing the whole page.

---

## 3. Four design principles

### 1. State is data, not memory

Agents getting killed is normal, not exceptional. Progress must be written to disk in a crash-safe way: temp file plus atomic rename, so a reader never sees a half-written JSON.

A detail worth calling out: **progress snapshots and final commits need different durability policies.** fsync every 500ms costs about half a second of pure waiting over a 1GB download, and losing one intermediate snapshot is harmless — you just roll back a little. So intermediate progress uses atomic writes without fsync, and only the final commit syncs. Layer by data semantics, not one-size-fits-all.

### 2. Failure is explicit; never leave a half-product

The most dangerous behaviour of a downloader is not failing. It is **succeeding with a corrupt file**.

Here is a real path: a task is interrupted, leaving a `.part` file; the signed URL rotates, the task ID changes, so it becomes a "new task"; the new task doesn't recognize that `.part`; aria2 starts with `--continue=true`, treats the existing bytes as an already-downloaded prefix, and publishes a file mixed with stale bytes — or entirely zeros — to the target path, returning `ok: true`.

The agent cannot tell that file is wrong unless it holds an external digest. This is why "explicit failure" is a design constraint, not a slogan:

- In-flight bytes live in `<target>.part`; the target path appears only after verification;
- Fresh tasks clean up orphaned fragments that do not belong to them;
- `resumed: true` is reported only when bytes were actually reused;
- A cached file that fails verification is **renamed** to `.corrupt` to preserve evidence, not silently deleted;
- Without an external checksum, report `integrity: "unverified"` — "transfer and size were checked, but I cannot prove these are the source's correct bytes."

That last one matters most. It turns an implicit lie into an explicit trust level. The agent may accept it — but it is no longer misled.

### 3. Content never enters context

An agent's context window is scarce. A 293KB HTML page pushed into it costs roughly 70,000 tokens — more than half of a 128k window in one shot. A summarization tool appears to save tokens, but returns a lossy projection: the bytes were never stored, and asking about a second section means reprocessing the whole page.

A download layer separates acquisition from consumption:

- `fetch` puts full bytes on disk and returns a few hundred bytes of JSON;
- `read --grep` pulls only the requested lines, with context, under a hard cap (256KB by default);
- For deeper work the agent uses grep/python directly on the file instead of pouring it into the conversation.

Measured: caching a 293KB page costs about 140 tokens up front (vs ~73,000), then ~1,000 tokens per targeted query. That is an order-of-magnitude difference — and the content is permanently on disk, verifiable, and reusable offline.

### 4. Zero config, and honest defaults

Agents will not read a settings panel. So:

- Mirrors are probed automatically: parallel `Range: bytes=0-0` requests, ranked reachable > range-capable > low TTFB, with failover to the next-best if the primary dies mid-download;
- Backends are chosen automatically: pure Go first (no subprocess overhead), external downloader as fallback;
- Probe results are reused for 60 seconds — an agent killed mid-download should not pay the probe cost again when it re-runs the same command.

But "zero config" does not mean "silent." When a fallback is unavailable, an orphan fragment is discarded, or a cached file's size disagrees with the record, that belongs in `warnings`. Agents may not read settings panels, but they can read structured warnings.

---

## 4. A real case: the 1.9GB lesson

On 2026-11-27 an agent set out to download a 3GB Qwen2.5-1.5B model. It hit three problems, each worth its own paragraph.

**Signed URL identity.** The source was a short-lived signed URL on `cas-bridge.xethub.hf.co`, about an hour to live. The first attempt with the native backend failed at 1.9GB with a TLS handshake timeout. By the time it restarted, the URL had rotated — `Expires`, `Signature`, `X-Amz-*` all changed. Since the task ID was derived from the URL, the refreshed link became a brand-new task and nobody claimed that 1.9GB. A full re-download with aria2 took 35 minutes.

**Concurrency granularity.** For a 3GB file, the old implementation made 16 fixed chunks of ~193MB. One slow connection could hold the entire tail hostage, and the resume unit was 193MB.

**Hanging.** A 30-second no-data timeout does not stop a connection that dribbles a few bytes every few dozen seconds — it keeps resetting the timer, leaving a task hung for hours without an error.

What these three have in common: **none is "the downloader isn't fast enough." They are failures to express failure and recovery semantics correctly.** The fixes map one-to-one: strip signature parameters from task identity (plus a `--task-key` escape hatch for unrecognized private sources); replace fixed chunks with 2–16MB work units in a 16-worker queue; add a 10-second window / 8KiB/s rate watchdog.

Local benchmark (256MB, single connection throttled to 1MB/s): 16.75s → 4.40s, 3.8×. But the number is less important than what now happens on failure: it stops at a defined place, says why, and continues from there on the next call.

---

## 5. Why now

Agents are moving from toys to production. When an agent only runs 30-second demos, curl is enough. When it needs to:

- pull multi-GB model weights;
- cache large documents for multi-round analysis;
- keep working through sessions that get interrupted, compacted, and restarted;

"download" stops being an action and becomes a lifecycle that must be managed.

I expect every serious agent runtime will eventually need this layer. It may not remain a standalone tool — it may be absorbed into the agent's filesystem abstraction. But the contract will be the same: **durable state, explicit failure, no content in context, verifiable trust.**

OpenDrain is one concrete implementation of that contract — pure Go standard library, zero dependencies, single binary. But the point of this article is not "use OpenDrain." It is this: **if your agent is still calling curl directly, what it lacks is not a better downloader. It lacks a download layer.**

---

*All figures come from OpenDrain's [BENCHMARK.md](https://github.com/yoyo636/opendrain/blob/main/BENCHMARK.md) and real task state records. Project: [yoyo636/opendrain](https://github.com/yoyo636/opendrain).*
