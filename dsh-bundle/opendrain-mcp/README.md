# DSH bundle: OpenDrain MCP

A configuration-only DeepSeek Harness bundle that connects the OpenDrain
Download Action Layer to the agent as MCP tools.

## What it adds

| DSH tool | Purpose |
|---|---|
| `mcp__opendrain__download` | the only entry point for external bytes |
| `mcp__opendrain__manifest` | list files previous sessions downloaded |
| `mcp__opendrain__verify` | re-hash one managed file before trusting it |
| `mcp__opendrain__read_excerpt` | bounded text read (grep/head/tail) |

## Install

```sh
# 1. install the binaries (v0.3.0+ needs `opendrain dap`)
go build -trimpath -ldflags="-s -w -X main.version=0.3.0" -o ~/.local/bin/opendrain .
go build -trimpath -ldflags="-s -w" -o ~/.local/bin/opendrain-mcp ./cmd/opendrain-mcp
```

Then in DSH: **Plugins → Add plugin → local directory**, pointing at this
folder. Or from an agent with `plugin_manager`:

```
plugin_manager(action: install_bundle, target: <abs path to this dir>)
```

## Files

- `package.json` — bundle manifest (`dsh.bundle.patch`)
- `cordis.patch.yml` — inserts `@deepseek-ai/dsh-mcp-client` with stdio transport
- `locale/en.json`, `locale/zh-CN.json` — display title/description
- `docs/DETAIL.md` — detail page
- `icon.svg` — 36×36 icon

## Verify

After install, `list_plugins` shows `include:mcp-opendrain-dal` as
`enabled: true, fiberPhase: active`, and the process tree shows
`opendrain-mcp` as a child of the DSH host with `opendrain dap` under it.
Tools appear in **new** sessions (the profile is composed at session start).

## Uninstall

Remove the bundle from the profile (Plugins → remove), or delete the
`mcp-opendrain-dal` row from the profile patch.