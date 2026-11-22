---
name: fetch
description: Compatibility alias for the opendrain skill. Download bytes to disk with resumable state and verified integrity, returning one small JSON object instead of flooding context. Use `opendrain manifest` at session start and `opendrain read` to inspect large files.
---

# fetch (alias)

This skill is an alias for `opendrain`. The full lifecycle contract —
manifest / fetch / verify / read / status / clean — lives in the
`opendrain` SKILL.md. Install it with `opendrain install`.

For a single download:

```sh
opendrain fetch --url URL --target PATH [--checksum sha256:HEX] [--mirror URL]...
```

The target path only ever holds a complete, verified file; bytes in flight
live in `<target>.part`. After any interruption, re-call the same command to
resume. Stdout is one JSON object; content never enters the conversation.
