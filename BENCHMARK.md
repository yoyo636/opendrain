# OpenDrain Before/After 基准报告

> 测试日期：2026-10-27/28 · 环境：macOS 27.0 arm64 · 网络：家庭宽带（对 HF 等站点不可达，dl.google.com / USTC / MDN 可达）
> 被测版本：OpenDrain v0.1（Go 1.23.4 编译，单二进制 ~2.8MB，零第三方依赖）
> aria2c 1.37.0（Homebrew）作为 aria2 后端

---

## 场景 1：Agent 缓存 MDN 大文档页供后续分析

**任务**：把 [MDN HTTP Headers 索引页](https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers)（293,636 字节 HTML）缓存到本地，之后查询某个 header 的语义。

### Before：context 内消化

| 方式 | 消耗 | 结果 |
|---|---|---|
| `curl -sL <url>` 直接进 context | 293,636 字符 ≈ **~73,000 tokens**（按 chars/4 估算） | 全量 HTML 进 context，单页即占 128k 窗口的 **57%**；两个页面必然触发 compaction |
| `webfetch`（真实执行一次，prompt="列出所有 header 名"） | 工具结果 ~2,714 字符 ≈ **~680 tokens** | 拿到的是**有损投影**：172 个 header 名。底层 293KB HTML 全量经过了抓取+转换+小模型管道；字节从未落盘。想要页面其它信息（某 header 的参数表、示例），必须再发起一次完整往返重新处理整页 |
| 是否触发 context 溢出 | curl 方式：一页即接近危险水位；webfetch 方式：不溢出，但**以失去完整内容为代价**，且每次提问都重复支付整页处理成本 | |

### After：OpenDrain fetch

```sh
opendrain fetch --url https://developer.mozilla.org/en-US/docs/Web/HTTP/Headers \
  --target ~/.cache/opendrain-bench/mdn-http-headers.html
```

| 指标 | 数值 |
|---|---|
| 进入 context 的内容 | 结果 JSON **557 字节 ≈ ~140 tokens**（含 sha256、镜像探测、耗时） |
| 耗时 | **780 ms**（auto→native；aria2 后端为 4,085 ms，进程开销主导，已对小文件自动路由 native） |
| 文件完整性 | 293,636 字节，与 curl 独立下载**逐字节一致**；sha256=`46ce52cada89…` 随结果返回 |
| 二次调用（缓存命中） | **6 ms**（重新哈希校验后直接返回，幂等） |
| 按需读取 | 用 grep/python 从磁盘取 `Accept-Ranges` 段落：**~997 tokens**，想要几段取几段，文件可离线复用 |

**结论**：同等信息获取下，首次消耗 140 tokens vs ~73,000 tokens（**~500×**）；且 after 模式下内容永久落盘、可校验、可按需精读，而 before 模式每次提问都要重付整页成本。

---

## 场景 2：28MB 发布包下载 + 外部权威 sha256 校验

**任务**：下载 `go1.23.4.src.tar.gz`（28,177,188 字节），用 go.dev 官方公布的 sha256 校验。

| 实现 | 耗时 | 有效带宽 | 完整性 |
|---|---|---|---|
| `curl` 单连接基线 | 6.67 s | 4.23 MB/s | — |
| OpenDrain native（4 分片并发，auto 探测后选 dl.google.com） | 8.24 s（含探测 0.4s） | 3.42 MB/s | sha256 = `ad345ac4…3531`，**与 go.dev 官方一致 ✅** |
| OpenDrain aria2（8 连接） | 9.63 s（含进程+RPC 开销） | 2.93 MB/s | sha256 同上 **✅** |

> 诚实记录：在本网络路径上多连接**没有**跑赢 curl 单连接（运营商对单连接已给足带宽，多连接引入分片协调开销）。OpenDrain 的价值主张在 Agent 维度：零 token 消耗、状态可续传、强校验、结构化错误——速度在后端（aria2/镜像）可用时自动取最优，不迷信多连接。

### 双镜像探测（真实运行输出）

dl.google.com 与 USTC 镜像同时给到 `fetch`，探测结果（节选自结果 JSON）：

```
dl.google.com/go/go1.23.4.src.tar.gz   latency_ms: 419  range: true  status: 206  ← 选中
mirrors.ustc.edu.cn/golang/…           latency_ms: 432  range: true  status: 206
```

择优规则生效：双源均支持 Range，取 TTFB 更低者为主源，次优保留为故障切换源。

---

## 场景 3：中断续传（Agent 生命周期核心场景）

### 3a. 同后端真续传（aria2 → aria2）

```
第 1 次：opendrain fetch … --backend aria2
  └─ 4.0s 时 SIGINT（模拟 compaction/超时杀进程）
  └─ {"status":"cancelled","error":{"code":"cancelled","message":"…re-call to resume"}}
  └─ 磁盘状态：done=6,537,216 / 76,280,703，.aria2 控制文件保留

第 2 次：同参数重调
  └─ {"ok":true,"resumed":true,"backend":"aria2"}   ← 24.1s 完成（75MB 文件）
  └─ sha256 与 go.dev 官方一致 ✅
```

### 3b. 跨后端切换 + 校验层拦截真实损坏（值得记录的插曲）

```
第 1 次：--backend native，4s 时 SIGINT，落盘 16.9MB（native 稀疏分片格式）
第 2 次：auto 选中 aria2
  └─ 引擎检测到后端切换 → native 残片对 aria2 不可用 → 诚实重启（resumed 不虚报）
  └─ 完成，sha256 = 官方值 ✅
```

**修复前发生过一次真实损坏并被校验层当场拦截**：native 预分配的稀疏 part 文件被 aria2 的
`continue` 语义当作"已下载前缀"，产出混入空洞的文件 →
`{"error":{"code":"checksum_mismatch","expected":"sha256:64a3fa…","actual":"sha256:262fee…"}}`，
**part 文件被删除，目标路径始终没有坏文件**。用 curl 独立全量下载两个镜像比对，确认镜像内容
与官方一致，损坏来自引擎跨后端复用残片 → 修复（后端切换时作废不兼容残片）。这正是
"失败要显式、绝不留下半成品"设计的意义：损坏被结构化错误暴露，而不是静默产出坏文件。

---

## 失败显式性抽检

- `--backend aria2` 且系统缺 CA 证书包（修复前）：`{"code":"backend_failed","detail":{"aria2_error_code":"1"},"message":"SSL/TLS handshake failure: unable to get local issuer certificate"}` —— 结构化、可归因，无静默降级。
- 全部镜像不可达：`{"code":"no_mirror_reachable","detail":{"probes":[…]}}`。
- 畸形 checksum：`{"code":"bad_request","message":"checksum must be \"sha256:<64 hex chars>\""}`。

## 汇总

| 维度 | 传统方式（curl/webfetch） | OpenDrain |
|---|---|---|
| token 成本（缓存一个 293KB 页面） | ~73k（进 context）或 ~680 + 每问一次重付（webfetch） | **~140，之后按需 ~1k/次** |
| 内容保真 | webfetch 有损；curl 全量但挤爆 context | 磁盘全量，sha256 可验证 |
| 进程被杀 | 从头再来 | **重调即续传**（同后端 `resumed:true`） |
| 坏文件风险 | 无校验则静默接受 | checksum 不匹配即删除并报错 |
| 镜像 | 手动挑 | 自动探测择优 + 中途故障切换 |

### 已知限制（诚实记录）
- 小文件上 aria2 的进程开销明显（4.1s vs native 0.85s）——auto 模式已对 <8MB 文件自动路由 native；更大优化空间留给后续迭代。
- 本网络路径下多连接未跑赢 curl 单连接（见场景 2 说明）；速度收益依赖真实瓶颈是"单连接带宽"的路径。
- webfetch 的 token 消耗只能按结果字符数估算（底层管道成本不可见），已标注方法学。

---

## 2026-11-22 性能优化：分片与探测复用

**方法**：本地 Range 服务器，每连接限速 12 MB/s（模拟 single-connection throttled host，即多连接真正能获益的场景）。每档跑 2 次取最优，每次清空状态与目标文件。二进制用 `CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 -trimpath -ldflags="-s -w"` 编译。

| 文件 | 优化前 native | 优化后 native | curl 单连接 | 相对 curl 的加速比（前 → 后） |
|---|---|---|---|---|
| 28 MB | 1.231 s（**2 分片**） | **0.254 s**（14 分片） | 2.349 s | 1.91× → **9.25×** |
| 100 MB | 1.551 s（8 分片） | **0.733 s**（16 分片） | 8.348 s | 5.38× → **11.4×** |

**做了什么**

1. `targetChunk` 16MB → 2MB、上限 8 → 16，且不允许 <1MB 的碎片。28MB 文件从 2 分片变 14 分片。
2. `copyChunk` 缓冲区 128KB → 512KB；`FileSha256` 用 1MB 显式缓冲替代 `io.Copy` 默认 32KB。
3. 续传时复用 60 秒内、URL 集合一致且全部健康的探测结果 —— 跳过 6 秒探测预算，这是"Agent 被杀后重调同一命令"的响应延迟大头。
4. auto 路由改为优先 native：本机实测 native 0.254/0.733 s vs aria2 3.545/4.009 s（aria2 进程+RPC 开销与 8 连接限制），原先 auto 选 aria2 反而慢 5–14×。aria2 提到 16 连接并保留为 fallback 与显式选项。
5. 修正取消语义：ctx 被取消时不再启动 fallback 后端（原先会把 `cancelled` 覆盖成误导性的 `backend_failed`）。

**没有做**

- 缓存命中不改成 size+mtime 快速路径：虽然能省 ~70ms（100MB），但那会让 Agent 拿到"记录里的 digest"而不是"磁盘上字节的 digest"，违背校验设计原则。
- 没有无限提高并发：16 是实测收益与"不锤小服务器"之间的折中；链路已饱和时更多连接无增益（见场景 2）。

**已知限制**：本测试是单机回环 + 服务端限速，反映的是 per-connection throttled 场景；链路饱和或服务器不支持 Range 时不适用，请按场景 2 的方法先判瓶颈。


---

## 2026-11-27 大文件 (>1GB) 专项

**背景**：上一轮 GitHub 专项把 GitHub 域名统一限制到 4 分片（针对 5.9MB 小文件的 CDN 抖动）。这对 GB 级资产是硬伤——1GB 文件只有 4 路并发，慢链路上要几分钟。

**本地基准**（1GiB 文件，Range server 每连接限速，macOS APFS）：

| 场景 | curl 单连接 | before | after | 备注 |
|---|---|---|---|---|
| 40MB/s/连接 | 25.61s | 3.65s | 3.18~3.71s | 本地 SSD 噪声大于优化收益 |
| 8MB/s/连接 | 127.99s | 9.18s | 9.10s | 同上 |

**诚实结论**：本地 SSD 上这些优化收益很小（fsync 573ms 只占 3.65s 的 15%，且 SSD 上被缓存掩盖）。真正的问题是**真实网络/慢盘路径**，本地基准无法复现。

**改动**（每项都有微基准或测试支撑）：

1. **GitHub 大文件恢复满并发**：`githubSmallChunks=4` 只对 <256MB 生效，1GB GitHub 文件用满 16 分片（`TestWorkerCountLargeFilesAndGitHub` 覆盖阈值两侧）。
2. **进度保存去 fsync**：新增 `Store.PutProgress`（tmp + 原子 rename，无 fsync），1GB 下载原本每 500ms 一次 fsync（微基准 60 次 = 573ms）。最终提交仍用带 fsync 的 `Put`，崩溃续传语义不变。
3. **大文件进度间隔 500ms → 2s**：减少状态写入次数。
4. **进度计数批量发布**：每累计 4MB 才抢一次共享锁（1GB 从 2048 次锁降到 256 次；微基准 72ms → 51ms）。

**语义不变**：崩溃续传、sha256 校验、原子发布、状态文件一致性全部保留；`PutProgress` 的原子性由 `TestPutProgressIsAtomicAndReadable` 覆盖。

---

## 2026-11-28 大文件慢连接 / 挂死专项

**背景**：用户反馈大文件"特别慢"。上一轮把大文件从 4 分片恢复到 16 分片，但仍然把
3GB 文件切成 16 个 ~193MB 的固定分片：一条抖动/限速的连接会拖住整个尾巴，续传粒度
也只有 193MB。另一个真实问题是 30 秒无数据超时挡不住"每几十秒滴几个字节"的连接，
任务可以挂几个小时。

**改动**

1. **工作队列 + 小工作单元**：`workerCount` 仍决定并发（最多 16），但 `rangedChunks`
   把文件切成约 `total/(workers*4)` 大小的单元，夹在 2–16MB 之间。3GB 从 16×193MB
   变成约 192×16MB；256MB 从 16×16MB 变成 64×4MB。worker 动态领取，慢连接只拖一个单元，
   完成的单元立即被其他 worker 接走。
2. **速率看门狗**：30s 无数据仍会断开；新增 10s 窗口、低于 8KiB/s 就中止该连接（关闭
   body + cancel context），重试/换镜像，避免滴流连接把任务挂死。
3. **非 Range 中断恢复**：整文件 200 响应允许从 0 重启（旧分支不可达，导致永久
   `all_mirrors_failed`）；非 Range 任务不再虚报 `resumed`。

**本地基准**（256MB，Range server；旧=0.1.0，新=0.2.0；`--backend native`，含探测与
sha256 校验；每档 2 次取较优）：

| 场景 | 旧实现 | 新实现 | 变化 |
|---|---|---|---|
| 16 连接等速（无慢连接） | 400–524ms | 393–402ms | 无回退 |
| 1 条连接限速 1MB/s | **16,749ms** | **4,396ms** | **3.8×** |

**解释**：等速时两者都受环回/磁盘限制；慢连接场景的差距来自分片粒度。旧实现慢连接
拿到 16MB 的一个整片，其他 15 条连接做完后只能等它；新实现慢连接只拿 4MB 的单元，
其余 worker 把剩余单元清空，尾巴从 16MB 降到 4MB。3GB/16MB 单元的尾巴是旧实现的
1/12，理论上收益更大。

**真实网络抽检**

- `dl.google.com` 76MB：旧 2.08s，新 1.80s（单次；此前同一命令曾因网络路径异常挂住
  39 分钟，状态停在 4.7MB，说明挂死看门狗是必要的）。
- HuggingFace/xet（`hf-mirror.com` 302 → `cas-bridge.xethub.hf.co`）：
  - 单连接 64MB：2.01s ≈ 32MB/s；
  - 16 路并行 128MB：约 2.45s ≈ 52MB/s。
  - 但同一模型 3GB 任务在 2026-11-27 曾只有 ~1.5MB/s（native 1.9GB/19min，aria2 3.09GB/34.5min）。

**结论**：代码侧能修的是"慢连接拖尾、分片粒度、滴流挂死、非 Range 恢复"；**物理带宽
和 CDN/运营商路径不在客户端控制范围内**。遇到"慢"先用同 URL 的 curl 单连接 + 16 路
并行 Range 各测一次：如果单连接就慢，或 16 路不聚合，瓶颈在路径/服务端，不在 OpenDrain。
