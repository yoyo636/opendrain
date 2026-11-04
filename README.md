# OpenDrain — Agent-native 下载层

以 Opencode skill 形态存在的文件 I/O 原语：`fetch(url, target, checksum?, mirrors?, resume?)`。

普通下载器的用户是人；OpenDrain 的用户是 Agent——它可能在任意一步被杀（compaction/超时），
所以状态落盘可续传；它不"打开看看"，所以完整性必须强制校验；它不读设置面板，所以零配置；
它调用下载的时机是代码执行中途，所以内容**永远不进 context**，只回一行 JSON。

## 安装

### npm（跨平台，自动拉取对应平台的预编译二进制）

```sh
npm install -g opendrain      # 或直接 npx opendrain fetch --url ... --target ...
opendrain install             # 把 fetch skill 装进各 agent 的 skills 目录
```

npm 包是零依赖的纯 JS 启动器：`postinstall` 从 GitHub Release 拉取当前平台的二进制；
拉取失败也不会让安装中断，launcher 会在首次运行时重试。包内同时附带 `skills/fetch/SKILL.md`。

> npm 12 起默认拦截依赖的安装脚本。若想在安装时就下载好二进制（省掉首次运行的等待）：
> `npm install -g --allow-scripts=opendrain opendrain`，
> 或 `npm config set allow-scripts=opendrain --location=user`。
> 不加也能用——launcher 会在第一次执行时自动补下二进制。

### 源码

```sh
go build -o opendrain .        # 纯 Go 标准库，零第三方依赖
./opendrain install            # skill 装入 ~/.config/opencode/skills/fetch 等，二进制复制到 ~/.local/bin
```

### 从源码构建各平台二进制

```sh
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o dist/opendrain-darwin-arm64 .
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/opendrain-linux-amd64  .
```

或推 `v*` tag 触发 `.github/workflows/release.yml` 自动构建并发布 Release 资源。

## 使用

```sh
opendrain fetch --url URL --target PATH \
  [--checksum sha256:HEX] [--mirror URL]... [--no-resume] [--backend auto|aria2|native]
opendrain status --url URL --target PATH
opendrain clean (--url URL --target PATH | --id ID | --all)
```

- 结果：stdout 一行 JSON（`ok / status / bytes / sha256 / backend / selected_mirror / resumed / warnings / error{code}`）。
- 目标路径只出现完整且校验过的文件——下载中字节在 `<target>.part`，校验通过后原子改名。
- 任何中断后**重调同一命令即续传**；状态在 `~/.opendrain/state/<sha256(url|target)>.json`。
- 相对路径与绝对路径指向同一目标时 ID 相同，`fetch` / `status` 始终算出同一个任务。
- 状态文件损坏不会让任务卡死：`fetch` 自动隔离坏文件并从头开始（`warnings` 里说明），
  `status` 报 `state_corrupt`，`clean` 可显式清掉状态。

## 架构

```
main.go                      CLI（fetch/status/install，go:embed 内嵌 SKILL.md）
internal/core
  fetch.go                   编排：校验→探测→选后端→下载→sha256→原子发布
  probe.go                   镜像探测：GET Range 0-0 测 TTFB/Range 支持，择优排序
                             （以实际响应为准：回 200 即视为不支持 Range，不信 Accept-Ranges 头）
  store.go                   状态落盘（tmp+fsync+rename 原子写）
  checksum.go / types.go
internal/backend
  aria2.go                   aria2c 子进程 + JSON-RPC（大文件首选，--stop-with-process 防孤儿）
  native.go                  纯 Go fallback：分片并发、逐 chunk 续传、30s stall 检测、镜像故障切换
```

- **auto 路由**：已知大小 <8MB → native（aria2 进程开销不划算）；否则 aria2 可用即用，不可用回退 native。
- **镜像**：探测排序（可达 > 支持 Range > TTFB 低）；主源中途失败自动切次优并保留已下载分片。
- **跨后端切换**：native 与 aria2 的残片格式不兼容，切换时作废旧残片诚实重下（防止把稀疏空洞当代价最优——见 BENCHMARK.md 场景 3b 的真实案例）。

## 测试

```sh
go test ./...    # 20 个用例：分片/sha、取消续传、镜像故障切换、校验失败零残留、
                 # 无 Range 回退、未知大小流式、缓存幂等、探测排序、假 aria2 RPC 协议、
                 # 谎报 Accept-Ranges 的服务器、残片丢失不虚报 resumed、状态文件损坏自愈、
                 # 跨进程任务锁、缓存文件校验失败保留 .corrupt
```

真实场景数据见 [BENCHMARK.md](BENCHMARK.md)。

## 可靠性修复（2026-10-30）

代码审查发现并修复的四个功能性缺陷，均已加回归用例：

| 问题 | 现象 | 修复 |
|---|---|---|
| 探针相信 `Accept-Ranges` 头 | 服务器无视 Range 却声明支持时，每次下载都 `all_mirrors_failed` | 以实际响应为准：探针拿到 200 即视为不支持 Range，退回单流下载 |
| `resumed` 虚报 | 残片丢失后重下，JSON 仍报 `resumed: true` | `resumed` 改到残片存活检查之后计算，无残片可续时为 false |
| 相对路径下 `status` 查不到任务 | `fetch` 与 `status` 算出两个不同 ID | ID 统一按绝对路径归一化，三处调用一致 |
| 状态文件损坏导致任务卡死 | 同一个命令永远返回 `internal_error`，无自救手段 | `fetch` 自动隔离坏文件并从头开始（`warnings` 说明），`status` 报 `state_corrupt`，新增 `opendrain clean` |

## 并发与文件安全（2026-11-04）

| 问题 | 修复 |
|---|---|
| 两个 Agent 同时 `fetch` 同一个 `(url, target)` 会互踩状态文件和 `.part` | 按任务 ID 加跨进程独占锁（`flock`），锁覆盖从读状态到原子发布的整个流水线；进程被杀时由内核自动释放，不会留下死锁文件。抢不到锁时按 `ctx` 取消，不无限阻塞 |
| 缓存文件校验失败时直接删除用户已有文件 | 改名为 `<target>.corrupt`（已存在则加时间戳后缀），路径写进 `error.detail.quarantined_as`，字节保留可复查；`.part` 临时文件仍直接删除 |

> Windows 上标准库没有 advisory `flock`，目前该平台不做跨进程串行化（状态写入本身仍是原子的）；Unix/macOS/Linux 完整生效。

## 范围外（后续迭代）

GUI、BT/种子、流媒体解析、P2P/CDN 调度、跨设备同步。
