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
