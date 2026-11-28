# Download Action Layer (DAL)

> 一种新类型的 Agent 组件：不是 skill，不是工具，而是**下载行动的接管层**。

## 定义

DAL 位于 Agent 的"意图"和"字节"之间。Agent 不直接调用 `curl`、`webfetch` 或某个下载函数；它表达一个**下载意图**，DAL 决定如何完成、如何恢复、如何验证，并只把一个小句柄交回给 Agent。

```
Agent ──意图──> DAL ──> 磁盘字节
       <─句柄──      （状态/摘要/警告）
```

它和相邻概念的区别：

| 概念 | 粒度 | 谁决定怎么下载 | 有状态 | 跨会话 |
|---|---|---|---|---|
| curl / wget | 命令 | Agent 自己 | 否 | 否 |
| skill | 提示词里的用法建议 | Agent 自己 | 否 | 否 |
| MCP tool | 一个可调用的函数 | Agent 调用时传参 | 通常否 | 否 |
| **DAL** | **一类行动** | **DAL 自己** | **是** | **是** |

关键差异只有一句：**skill 告诉 Agent 怎么做；DAL 替 Agent 做，并且记住做过什么。**

## 只管下载

DAL 的边界刻意收窄：

- ✅ URL → 本地文件
- ✅ 断点续传、镜像探测、校验、原子发布
- ✅ 下载状态、清单、缓存信任级别
- ❌ 不解释内容（不总结网页、不做 RAG）
- ❌ 不执行文件（不 build、不 run）
- ❌ 不管通用 shell / 写文件 / 编辑代码
- ❌ 不做认证代理（凭证只用于源站鉴权，不替用户管理登录态）

"接管整个 Agent 行动"在这里的准确含义是：**凡是下载行动，一律经过它**；不是接管 Agent 的全部行为。

## 接口：意图 → 句柄

```go
type Intent struct {
    URL      string   // 必需
    Target   string   // 必需
    Checksum string   // 可选，sha256:<hex>
    Mirrors  []string // 可选
    TaskKey  string   // 可选，签名 URL 轮换时的稳定身份
    Backend  string   // auto|native|aria2，默认 auto
    Resume   bool     // 默认 true
}

type Handle struct {
    ID        string
    Target    string
    Bytes     int64
    Sha256    string
    Integrity string   // checksum | unverified
    Resumed   bool
    Cached    bool
    Warnings  []string
    Status    string
    Error     *ErrInfo
}
```

`Act(ctx, intent)` 是唯一的入口。它保证：

1. 同一个意图重调 → 续传或缓存命中；
2. 目标路径只出现完整文件；
3. 返回句柄而不是内容；
4. 失败是结构化的，且不会留下半成品。

## 为什么这是"新类型"

不是因为代码多，而是因为**责任边界变了**：

- **工具**对一次调用负责；
- **DAL 对一个文件从第一次请求到最终被信任的全部生命周期负责**。

因此它必须拥有三样 skill 和 MCP tool 都不具备的东西：

1. **自己的状态存储**（跨进程、跨会话、崩溃安全）；
2. **自己的策略**（选镜像、选后端、切分片、判信任）；
3. **自己的语义**（什么叫完成、什么叫续传、什么叫可信任）。

Agent 只是说"我要这份字节"，剩下都是 DAL 的事。

## 接入方式（任意 harness）

DAL 不依赖任何 harness。接入点只有四种，按侵入性从低到高：

1. **工具层**：把 harness 的下载类工具直接实现为 `Act()`；
2. **拦截层**：hook `curl`/`wget`/`webfetch` 等调用，改写为 `Act()`；
3. **事件层**：监听 Agent 的"需要外部字节"事件，异步执行后回句柄；
4. **运行时层**：Agent 的文件访问走 DAL 挂载的虚拟文件系统。

当前 OpenDrain 已经实现的是第 1 种（CLI `opendrain act` 与可组合的 `action.Layer.Act()`）；第 2–4 种是各 harness 的适配工作。

### 作为 DSH 工具接入（当前环境）

在 DeepSeek Harness 中，接入方式是"工具层"：用 `dsh` 工具定义把 Agent 的下载调用指向 `opendrain act`（或内嵌的 `action.Layer`），让 Agent 只看到：

```
act(url, target, checksum?, task_key?) -> handle{id, target, bytes, sha256, integrity, resumed, cached, status}
```

工具描述要明确：这是下载行动的**唯一入口**；不要教 Agent 直接调用 curl/wget。这样"接管"就发生在工具定义层，而不是靠提示词劝告。

### 工具描述模板

```yaml
name: download
description: >
  Download bytes to a local file. This is the ONLY way to obtain external
  files; never use curl/wget/shell downloads. The layer persists state,
  resumes after interruption, verifies integrity, and returns a compact
  handle. Content never enters the conversation.
parameters:
  url: string (required, http/https)
  target: string (required, local path)
  checksum: string (optional, sha256:<hex>)
  task_key: string (optional, stable identity for rotating signed URLs)
  mirrors: string[] (optional)
returns: handle{id, target, bytes, sha256, integrity, resumed, cached, status, warnings, error}
```


## 与 OpenDrain 的关系

OpenDrain 是 DAL 的参考实现。`core.Fetch` 就是 `Act` 的完整语义；CLI、npm launcher、各 agent skill 只是不同的调用外壳。

换句话说：以前我们把 skill 当成产品；现在 skill 只是 DAL 的一种**适配层**。产品是那条下载行动总线本身。

---

## 接入你的机器：MCP 适配器

项目附带 `cmd/opendrain-mcp`：一个零依赖的 MCP server，把 DAP 暴露成 4 个工具
（`download` / `manifest` / `verify` / `read_excerpt`）。它自己启动 `opendrain dap`
子进程，所以 Agent 侧只需要配置一条命令。

安装（在当前机器上已完成）：

```sh
go build -trimpath -ldflags="-s -w -X main.version=0.3.0" -o ~/.local/bin/opendrain .
go build -trimpath -ldflags="-s -w" -o ~/.local/bin/opendrain-mcp ./cmd/opendrain-mcp
```

注册到各 Agent（CLI 支持内建命令的优先用它）：

```sh
claude mcp add --scope user opendrain-dal -- ~/.local/bin/opendrain-mcp
codex mcp add opendrain-dal -- ~/.local/bin/opendrain-mcp
opencode mcp add opendrain-dal -- ~/.local/bin/opendrain-mcp
kilo mcp add opendrain-dal -- ~/.local/bin/opendrain-mcp
```

Gemini / Cline / WorkBuddy 直接在各自配置的 `mcpServers` 里加：
`{"command": "~/.local/bin/opendrain-mcp", "args": []}`。

zcode 通过本地插件加载：在 `~/.zcode/cli/plugins/cache/opendrain-dal/local/0.1.0/`
放 `.zcode-plugin/plugin.json` 与 `.mcp.json`，内容同样是上面的 command。

最后，Agent 还需要一条"只走这个工具"的指令。把下面的内容放进各 Agent 的全局
指令文件（`~/.claude/CLAUDE.md`、`~/.codex/AGENTS.md`、`~/.config/opencode/AGENTS.md`
等）：

> All external file acquisition MUST go through the `opendrain-dal` MCP server's
> `download` tool. Do NOT use curl, wget, or shell downloads. At session start call
> `manifest`; before trusting a cached file call `verify`; use `read_excerpt` for
> bounded inspection. Exception: git clone / webfetch summarization / interactive login.
