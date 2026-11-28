---
name: fetch
description: Compatibility alias for the opendrain skill. Download bytes to disk with resumable state and checksum-or-explicit-unverified integrity, returning one small JSON object instead of flooding context. Use `opendrain manifest` at session start and `opendrain read` to inspect large files.
---

# fetch (alias)

This skill is an alias for `opendrain`. The full lifecycle contract —
manifest / fetch / verify / read / status / clean — lives in the
`opendrain` SKILL.md. Install it with `opendrain install`.

For a single download:

```sh
opendrain fetch --url URL --target PATH [--checksum sha256:HEX] [--mirror URL]... \
  [--task-key KEY]
```

The target path only ever holds a complete file; bytes in flight live in
`<target>.part`. After any interruption, re-call the same command to resume;
for a rotating signed URL, keep `--task-key` stable across calls. Stdout is
one small JSON object with `integrity: "checksum"` or `"unverified"`; content
never enters the conversation.
