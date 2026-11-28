# OpenDrain — Agent-native 下载层

以 Opencode skill 形态存在的文件 I/O 原语：`fetch(url, target, checksum?, mirrors?, resume?)`。

普通下载器的用户是人；OpenDrain 的用户是 Agent——它可能在任意一步被杀（compaction/超时），
所以状态落盘可续传；它不"打开看看"，所以字节必须可校验（有 checksum 时强校验，没有时
明确报告 `integrity: "unverified"`，而不是把 `ok` 当成"内容正确"）；它不读设置面板，
所以零配置；它调用下载的时机是代码执行中途，所以内容**永远不进 context**，只回一个小 JSON 对象。

## 核心保证（以及不保证什么）

| 保证 | 说明 |
|---|---|
| 目标路径不出现半成品 | 下载中的字节在 `<target>.part`；校验通过后才原子 rename |
| 有 checksum 时逐字节可信 | 不匹配则删除 `.part` 并返回 `checksum_mismatch`；缓存文件不匹配则隔离为 `.corrupt` |
| 无 checksum 时只保证传输/尺寸 | 结果 `integrity: "unverified"`；`sha256` 是"我拿到的字节"的摘要，不是源站正确性证明 |
| 中断可续传 | 同一任务重调即续传；`resumed:true` 只在字节确实被复用时出现 |
| 新任务不继承旧残片 | fresh 任务会删除同 target 的 orphan `.part`/`.aria2`，避免 aria2 把旧字节当前缀 |
| 缓存被截断不会冒充成功 | 记录尺寸与磁盘不符时隔离为 `.corrupt` 并自动重下 |

## GitHub 与签名 URL 专项

Agent 最常见的下载源是 GitHub，但短效签名 URL 远不止 GitHub；OpenDrain 对两者都做了处理：

- **凭据自动获取、不落盘**：优先读 `GITHUB_TOKEN` / `GH_TOKEN`，否则调用 `gh auth token`。token 只作为内存中的 HTTP header 发给 `github.com` / `api.github.com`；一旦重定向到 `release-assets.githubusercontent.com`，transport 会剥掉 Authorization，不会泄漏给 CDN。结果 JSON 与状态文件里都不含 token。`gh` 只在第一次遇到 GitHub URL 时懒加载，并有 5 秒超时。
- **短效签名 URL 可续传**：release asset 的 302 目标是带 `sig` / `se` / `jwt` 等参数的短效 URL（约 1 小时过期）。任务 ID 按剥离这些参数后的逻辑 URL 计算，链接刷新后重调同一命令仍能续传，不会丢分片。
- **HuggingFace / xet / S3 也识别**：`cas-bridge.xethub.hf.co`、`*.huggingface.co`、`*.hf.co` 与常见 S3/GCS 签名参数同样不计入任务身份；对无法识别的私有签名源，可用 `--task-key` 显式指定稳定任务键。这一条来自真实教训：一次 3GB 模型下载在链接刷新后变成了新任务，丢掉了 1.9GB 进度。
- **CDN 按大小自适应并发**：实测 GitHub CDN 小文件抖动大，所以 <256MB 的 GitHub 资产限 4 个 worker；**≥256MB（含 GB 级模型权重）自动恢复满 16 个**，避免大文件被小文件的保守策略拖垮。
- **安全重定向**：最多 10 跳，拒绝 https → http 降级。

实测（本机路径，5.9MB release asset）：通用 16 分片 116.9s → GitHub 4 分片 **75.6s**（快 35%），字节与 curl 下载逐字节一致。注意这条路径本身到 GitHub CDN 很慢，curl 单连接也要 67s；代码优化的是"别让慢路径更慢"，不是物理带宽。

## 大文件（>1GB）

- **16 个连接 + 工作队列，而不是 16 个巨大分片**：下载切成约 2–16MB 的工作单元，worker 队列动态领取。旧实现把 3GB 文件切成 16 个 ~193MB 的分片，一条慢连接会拖住整个尾巴；现在一条慢连接只拖住一个小单元。失败/断线的单元可以独立重试，续传粒度也从 193MB 降到 16MB。
- **慢连接不会永远挂住**：30 秒无数据触发断开；另加 10 秒窗口的 8KiB/s 速率看门狗，防止每秒滴几个字节的连接让任务卡死。
- GitHub 域名 256MB 以下限 4 个 worker（CDN 抖动），以上自动满 16 个。
- 进度保存每 2 秒一次且无 fsync（仍原子 rename），最终提交带 fsync；进度计数按 4MB 批量更新。
- 实测（本地 256MB、单连接限速 1MB/s）：旧实现 16.75s → 新实现 **4.40s（3.8×）**；等速场景无回退。详见 [BENCHMARK.md](BENCHMARK.md)。

## Agent 会话生命周期

OpenDrain 不只是下载器，而是 Agent 全程使用的文件层：

| 阶段 | 命令 | Agent 得到什么 |
|---|---|---|
| 会话启动 / compaction 后 | `opendrain manifest [--verify]` | 之前所有会话留下的文件、路径、sha256、是否还在磁盘上 |
| 需要新字节 | `opendrain fetch ...` | 文件落盘 + 小 JSON 对象；中断重调即续传 |
| 信任一个缓存文件前 | `opendrain verify [--target PATH]` | 重哈希对比，mismatch 时退出 1 |
| 检查大文件 | `opendrain read --target PATH --grep RE --context N` | 只看需要的几行，默认最多返回 256KB |
| 结束 / 下次继续 | （无需操作） | 状态留在 `~/.opendrain/state`，下次 manifest 可见 |

`opendrain install` 会把完整生命周期 skill 装到 OpenCode / WorkBuddy / Claude Code / Codex / zcode / Gemini CLI / Cline / Kilo / agents 的检测到的配置目录，同时保留 `fetch` 兼容别名（不覆盖已自定义的旧 skill）。

## 安装

### GitHub Release（当前推荐）

每个版本发布 5 个平台的预编译二进制（darwin/linux amd64+arm64、windows amd64）：

```sh
mkdir -p ~/.local/bin
curl -fL https://github.com/yoyo636/opendrain/releases/latest/download/opendrain-darwin-arm64 \
  -o ~/.local/bin/opendrain && chmod +x ~/.local/bin/opendrain
opendrain install             # 把 opendrain skill 装进检测到的 agent 配置目录
```

把 `darwin-arm64` 换成 `darwin-amd64` / `linux-amd64` / `linux-arm64` / `windows-amd64.exe` 即可。
Release 同时提供 `SHA256SUMS`，可用 `shasum -a 256 -c SHA256SUMS` 校验。

### npm（包已打包，尚未发布到 registry）

> 当前 npm registry 上还没有 `opendrain`（发布需要维护者执行 `npm login` 后 `npm publish`）。
> 在发布前请使用上面的 Release 二进制或源码安装；发布后下面的命令即可用。

```sh
npm install -g opendrain      # 或直接 npx opendrain fetch --url ... --target ...
opendrain install             # 把 skill 装进各 agent 的 skills 目录
```

npm 包是零依赖的纯 JS 启动器：`postinstall` 从 GitHub Release 拉取当前平台的二进制，
并用 Release 的 `SHA256SUMS` 校验（校验文件不可用时警告放行，校验不匹配则删除并拒绝）；
拉取失败也不会让安装中断，launcher 会在首次运行时重试。包内同时附带两份 SKILL.md。

> npm 12 起默认拦截依赖的安装脚本。若想在安装时就下载好二进制（省掉首次运行的等待）：
> `npm install -g --allow-scripts=opendrain opendrain`，
> 或 `npm config set allow-scripts=opendrain --location=user`。
> 不加也能用——launcher 会在第一次执行时自动补下二进制。

### 源码

```sh
go build -o opendrain .        # 纯 Go 标准库，零第三方依赖
./opendrain install            # skill 装入 ~/.config/opencode/skills/opendrain 等，二进制复制到 ~/.local/bin
```

### 从源码构建各平台二进制

```sh
VERSION=0.2.0
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o dist/opendrain-darwin-arm64 .
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o dist/opendrain-linux-amd64  .
```

或推 `v*` tag 触发 `.github/workflows/release.yml` 自动构建 5 个平台、生成 `SHA256SUMS` 并发布 Release。

### 维护者发布流程

1. 同步 `main.go` 的 `version`、`package.json` 的 `version`（当前 `0.2.0`）。
2. `git tag vX.Y.Z && git push origin main vX.Y.Z`；Actions 在 Linux runner 上交叉编译 5 个平台并上传 `SHA256SUMS`。
3. npm：在仓库配置 `NPM_TOKEN` secret 后，`npm-publish` workflow 会在 Release 发布时自动 `npm publish`（也可手动 `workflow_dispatch`）；或在本地 `npm login && npm publish --access public`。
4. 用 `shasum -a 256 -c SHA256SUMS` 校验产物，再 `opendrain install` 更新各 agent 的 skill。

## 使用

```sh
opendrain fetch --url URL --target PATH \
  [--checksum sha256:HEX] [--mirror URL]... [--task-key KEY] \
  [--no-resume] [--backend auto|aria2|native]
opendrain status --url URL --target PATH [--task-key KEY]
opendrain clean (--url URL --target PATH | --id ID | --all) [--task-key KEY]
opendrain verify --target PATH
opendrain --help | --version
```

- 结果：stdout 一份结构化 JSON（`ok / status / bytes / sha256 / backend / selected_mirror / resumed / cached / integrity / warnings / error{code}`）。
- `integrity` 为 `checksum` 表示与调用方给出的 sha256 一致；为 `unverified` 表示只完成了传输/尺寸校验，不要把它当成源站正确性证明。
- 目标路径只出现完整文件——下载中字节在 `<target>.part`，校验通过后原子改名。fresh 任务会先清掉同 target 的 orphan `.part`/`.aria2`。
- 任何中断后**重调同一命令即续传**；状态在 `~/.opendrain/state/<sha256(url|target)>.json`（目录 0700、文件 0600）。
- 相对路径与绝对路径指向同一目标时 ID 相同，`fetch` / `status` 始终算出同一个任务。
- 签名 URL 轮换（HF/xet/S3）时传 `--task-key`，或用已识别的 hub 域名；GitHub 的 `sig/se/jwt` 会自动规范化。
- 状态文件损坏不会让任务卡死：`fetch` 自动隔离坏文件并从头开始（`warnings` 里说明），
  `status` 报 `state_corrupt`，`clean` 可显式清掉状态、part 与 `.corrupt`（不会删除仍在运行的锁）。
- `verify --target` 只重哈希这一个文件，不再为了验一个 3GB 文件把整个 state 目录读一遍。

## 架构

```
main.go                      CLI（fetch/status/clean/manifest/verify/read/install，go:embed 内嵌 SKILL.md）
internal/core
  fetch.go                   编排：校验→锁→探测→选后端→下载→sha256→原子发布
  probe.go                   镜像探测：GET Range 0-0 测 TTFB/Range 支持，择优排序
                             （以实际响应为准：回 200 即视为不支持 Range，不信 Accept-Ranges 头）
  store.go                   状态落盘（tmp+fsync+rename 原子写；0700/0600）
  github.go / github_auth.go GitHub/HF 签名 URL 规范化、懒加载 token、安全重定向
  manifest.go / read.go / checksum.go / types.go
internal/backend
  aria2.go                   aria2c 子进程 + JSON-RPC（显式选择或 fallback，--stop-with-process 防孤儿）
  native.go                  纯 Go 主后端：worker 队列、2–16MB 工作单元、逐单元续传、
                             30s 无数据 + 8KiB/s 速率看门狗、镜像故障切换
```

- **auto 路由**：native 优先（最多 16 个 worker、零进程开销）；aria2 作为 fallback（仅在真的可用时启动），也可用 `--backend aria2` 显式选择。
- **镜像**：探测排序（可达 > 支持 Range > TTFB 低）；主源中途失败自动切次优并保留已下载分片。
- **跨后端切换 / 新任务残片**：native 与 aria2 的残片格式不兼容，切换时作废旧残片诚实重下；fresh 任务也不会继承旧 `.part`（这是 2026-11-28 修复的静默损坏路径）。
- **发布原子性**：`.part` → sha256/尺寸校验 → rename；缓存文件尺寸不符或校验失败时隔离为 `.corrupt`，不静默删除用户文件。

## 测试

```sh
go test ./...    # 45 个用例：分片/sha、取消续传、镜像故障切换、校验失败零残留、
                 # 无 Range 回退与中断重下、未知大小流式、缓存幂等/截断自愈、
                 # 探测排序、假 aria2 RPC 协议 + 真实 aria2 orphan part 回归、
                 # 谎报 Accept-Ranges 的服务器、残片丢失不虚报 resumed、状态文件损坏自愈、
                 # 跨进程任务锁、缓存文件校验失败保留 .corrupt、
                 # 签名 URL 规范化（GitHub/HF）、lazy token、--task-key、状态权限、
                 # verify --target、慢连接速率看门狗
```

真实场景数据见 [BENCHMARK.md](BENCHMARK.md)。

## 可靠性修复（2026-11-28）

> 更早的 2026-10-30 / 11-04 修复（探针不信 `Accept-Ranges`、`resumed` 不虚报、相对路径 ID 归一化、状态损坏自愈、全流程 flock）仍在，回归用例保留。

| 问题 | 现象 | 修复 |
|---|---|---|
| aria2 复用 orphan `.part` | fresh 任务（新 ID / clean 之后）把旧字节当已下载前缀；实测全零/带脏前缀的文件以 `ok:true` 发布 | fresh 任务启动前删除 `.part` + `.part.aria2`；真实 aria2 回归测试覆盖 partial/full-size 两种残片 |
| 缓存文件被截断仍报成功 | 6MB 文件截到 1MB，`fetch` 返回 `ok:true, cached:true` 和新 sha | 记录尺寸与磁盘不符时隔离为 `.corrupt` 并自动重下；结果加 `integrity` 字段 |
| 非 Range 服务器永远恢复不了 | 中断后重调返回 `all_mirrors_failed`（"returned http 200 ... start=..."），200 重启分支不可达 | 允许整文件 200 从 0 重启；非 Range 任务不虚报 `resumed`；加 `ignoreRange + cancel` 回归 |
| 签名 URL 身份只覆盖 GitHub | HF/xet 链接刷新后变成新任务，实测丢掉 1.9GB/19 分钟 | 扩展到 `*.huggingface.co` / `*.hf.co` / `xethub`；新增 `--task-key` 兜底 |
| fallback 掩盖真实错误 | `auto` 在 native 失败后启动未安装的 aria2，返回 "cannot start aria2c: exec: no command" | fallback 先检查 `Available`；两个都失败时保留主错误并把 fallback 错误写进 warnings |
| `clean` 残留 part / 不删 `.corrupt` | `--all` 不删 `.part`、`.lock`、`.json.corrupt`；note 说 part 会被覆盖 | clean 在任务锁保护下删除 state + part + control + `.corrupt`；运行中的任务报 `busy`；锁文件保留 |
| 状态文件权限过宽 / `--id` 可穿越 | state 目录 0755、文件 0644，且含预签名 URL；`clean --id ../../x` 可越界 | 目录 0700、文件 0600；`--id` 必须是 64 位小写 hex |
| `verify --target` 读全量 | 验一个 3GB 文件会先哈希目录下所有文件 | 用 `FindByTarget` 只哈希目标；未管理/缺失目标返回结构化错误 |

## 大文件性能（2026-11-28）

| 场景 | 旧实现 | 新实现 | 说明 |
|---|---|---|---|
| 本地 256MB，16 连接等速 | 0.40–0.52s | 0.39–0.40s | 无回退 |
| 本地 256MB，1 条连接限速 1MB/s | 16.75s | **4.40s** | 旧实现 16×16MB 尾巴；新实现 64×4MB 工作单元 |
| 真实 76MB（dl.google.com） | ~2.1s | ~1.8s | 路径快时两者一致 |
| HF/xet 64MB 单连接 | — | 2.0s（32MB/s） | 路径本身波动大；16 路 128MB 约 2.4s |

慢不一定在客户端：同一 HF 模型 3GB 任务曾在 1.5MB/s 路径上耗时 35 分钟，而另一次测
`cas-bridge` 单连接可达 32MB/s。现在客户端解决的是"慢连接/抖动拖尾"和"挂死"，不是物理带宽。

## 并发与文件安全（2026-11-04 / 2026-11-28）

| 问题 | 修复 |
|---|---|
| 两个 Agent 同时 `fetch` 同一个 `(url, target)` 会互踩状态文件和 `.part` | 按任务 ID 加跨进程独占锁（`flock`），锁覆盖从读状态到原子发布的整个流水线；进程被杀时由内核自动释放，不会留下死锁文件。抢不到锁时按 `ctx` 取消，不无限阻塞 |
| 缓存文件校验失败时直接删除用户已有文件 | 改名为 `<target>.corrupt`（已存在则加时间戳后缀），路径写进 `error.detail.quarantined_as`，字节保留可复查；`.part` 临时文件仍直接删除 |
| 状态文件可能含预签名 URL | 目录 0700、文件 0600；`clean` 可删除历史状态 |
| fresh 任务继承旧残片 | 启动前删除同 target 的 `.part`/`.aria2`，并在 `warnings` 说明 |

> Windows 上标准库没有 advisory `flock`，目前该平台不做跨进程串行化（状态写入本身仍是原子的）；Unix/macOS/Linux 完整生效。

## 范围外（后续迭代）

GUI、BT/种子、流媒体解析、P2P/CDN 调度、跨设备同步。
