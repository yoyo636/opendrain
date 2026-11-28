# OpenDrain Download Action Layer for DeepSeek Harness

Adds four MCP tools to the agent, namespaced as `mcp__opendrain__*`:

- `download` — the only entry point for fetching external bytes. Persists state,
  resumes after interruption, verifies integrity, returns a compact handle.
- `manifest` — list files downloaded by previous sessions.
- `verify` — re-hash one managed file before trusting it.
- `read_excerpt` — bounded text inspection (grep/head/tail, capped bytes).

The server runs locally from `~/.local/bin/opendrain-mcp` and talks to OpenDrain
over the Download Action Protocol (DAP v1). No credentials leave the machine;
tokens are only sent to their origin hosts by the download layer.
