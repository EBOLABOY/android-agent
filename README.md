# Android AI Agent (`android-agent`)

小米 13（`fuxi` / Android 16 / API 36 / HyperOS 3.0 / KernelSU 3.3.1）原生低延迟 AI Agent 底座。

- **当前状态**：**`v0.4.1` Codex 风格单步自观测执行循环、80 步滑动窗口折叠与防卡死接力升级版 — 在 `v0.4.0` 四栏 60fps 移动控制台与长轮询底座之上，为内置 Brain 引入「动作即观测」单步回传 `[操作后最新界面]`（步数与往返延迟直降 50%）、安全上限提升至 `80` 步配合最近 2 帧 UI 树滑动窗口折叠（第 50 步仍保持 ~2–3k 极简上下文）、周期 1/2/3 循环与 3 次同指纹卡死自愈提示（6 步无进展提前熔断），以及无工具流式检查点总结与控制台一键 `[继续执行剩余步骤]` 接力栏**。
- **架构亮点**：零外部 Go 依赖静态单文件（内嵌 `_ "time/tzdata"`），内置标准 **MCP Streamable HTTP (`2025-03-26`)** 服务端（`28` 个工具，新增 `phone_open_deeplink` 与 `phone_tap_and_type` 复合工具降延迟 ~40%，内置 LLM 大脑自动隐藏 `agent_pause`/`agent_resume` 防自锁）、懒加载 `app_process64` 微内核（3 分钟空闲销毁 + 杂讯隔离 + 王者荣耀秒杀熔断 + 焦点通知通道）、`evdev` 亮屏解锁门禁、2MB 进程内日志轮转、严格 Origin/Host 防护与本机 `127.0.0.1:8765` 默认监听。

---

## 1. 项目目录结构

### 宿主机代码（`/root/android-agent`）

```text
/root/android-agent/
├── cmd/agentd/main.go                 # 守护进程入口（配置加载、信号处理、CDP 探测、风控调度）
├── internal/
│   ├── config/config.go               # config.json 解析、环境变量覆盖、首次启动自动生成 64 位随机 Token
│   ├── backend/
│   │   ├── backend.go                 # DeviceBackend 抽象接口
│   │   ├── shell.go                   # hybrid-layered-v2 混合分层调度器（UI 树 + evdev + Shell/截屏兜底）
│   │   ├── microkernel.go             # 懒加载 app_process64 (uid=2000) JSONL 管道管理器（3min 空闲退出+游戏熔断）
│   │   ├── uitree.go                  # 控件树提纯压缩（300~500 Token）、卡片合并与 ID->坐标缓存
│   │   ├── touch_linux.go             # 纯 Go /dev/input/eventX Multitouch Type B 拟人触控（defer 防卡死释放）
│   │   ├── touch_ptr.go               # Linux ioctl 指针辅助
│   │   ├── uitree_test.go
│   │   ├── touch_test.go
│   │   └── cliphelper.jar             # 内嵌 Dalvik 微内核 JAR（AgentClip + AgentKernel，//go:embed）
│   ├── imgutil/
│   │   ├── compress.go                # 原始 RGBA / PNG 解码 + 双线性等比缩放(720px) + JPEG(q=70) 压缩
│   │   └── compress_test.go
│   ├── guard/
│   │   ├── guard.go                   # 游戏防风控：/proc 零子进程预检，王者荣耀进前台直接关闭 8765 监听
│   │   └── guard_test.go
│   ├── cdp/
│   │   ├── cdp.go                     # 直连抽象 Unix Socket (@chrome_devtools_remote + 自动探测 *_devtools_remote*)
│   │   └── cdp_test.go
│   ├── workflows/                     # 0-Token 原生日常工作流引擎（Edge/Bing 0截屏 CDP + 京东深链农场/刮京豆）
│   │   ├── workflows.go
│   │   └── workflows_test.go
│   ├── brain/                         # 本机内置 LLM 大脑（OpenAI/Gemini 双协议流式 SSE + Run Replay + 超级岛控制器 + ToolExecutor）
│   │   ├── llm.go
│   │   ├── agent.go
│   │   ├── island.go                  # 零 Hook HyperOS 3.0 超级岛 & 静默焦点通知控制器（350ms 节流 + 3s 自动收起 + 熔断秒消）
│   │   ├── memory.go
│   │   ├── scheduler.go
│   │   ├── island_test.go
│   │   └── brain_test.go
│   ├── webui/                         # 手机端暗黑模式移动 Web 控制台（//go:embed ui.html，支持添加到桌面）
│   │   ├── webui.go
│   │   └── ui.html
│   └── mcp/
│       ├── server.go                  # 标准 MCP Streamable HTTP (2025-03-26) + /ui + /api/v1/chat/{stream,events,cancel} + /api/v1/jobs/*
│       └── server_test.go
├── cliphelper/
│   ├── AgentClip.smali                # 适配 Android 16 IClipboard 的单次兜底剪贴板助手
│   └── AgentKernel.smali              # 常驻懒加载微内核（UiAutomation 内存抓树 + 剪贴板，JSONL IPC）
├── ksu-module/                        # KernelSU 模块打包目录
│   ├── module.prop
│   ├── service.sh                     # 开机自启与守护进程保活脚本
│   ├── action.sh                      # KSU 管理器一键查看/拉起脚本
│   ├── uninstall.sh                   # 卸载清理脚本
│   ├── config.json.example
│   ├── webroot/index.html             # KernelSU 管理器原生内置 OLED AI Agent WebUI
│   └── bin/
│       ├── ksu_api.sh                 # KernelSU Root 免密 curl 桥接脚本
│       └── agentd                     # linux/arm64 静态编译产物（.gitignore 忽略，运行 build.sh 生成）
├── lsposed-webview-debug/             # 极薄 13KB 零权限 LSPosed 模块（仅为选定 App 开启 WebView CDP）
│   ├── AndroidManifest.xml
│   ├── build_apk.sh                   # aapt2 + smali + zipalign + apksigner 一键打包签名
│   └── smali/com/agent/webviewdebug/
├── skills/
│   ├── ms-rewards-daily/              # Microsoft Rewards 每日全自动积分技能（Edge CDP + Bing WebView CDP 0截屏）
│   │   ├── SKILL.md
│   │   └── scripts/rewards_runner.py
│   └── jd-daily/                      # 京东每日极速任务与自动优质评价技能（WebView CDP 东东农场 + 领京豆 + 百字 5 星自动好评）
│       ├── SKILL.md
│       └── scripts/jd_runner.py
├── scripts/
│   ├── build.sh                       # 一键汇编 cliphelper.jar + go test -race + ARM64 静态交叉编译
│   └── daily_all.py                   # 一键串联每日微软积分 + 京东全套任务（全链路 WebView CDP，~4.5 分钟）
└── README.md
```

### 手机端运行目录

- **KernelSU 模块目录**：`/data/adb/modules/android_agent/`（`module.prop`、`service.sh`、`action.sh`、`uninstall.sh`、`bin/agentd`，统一权限与 `u:object_r:system_file:s0` SELinux 标签）
- **持久化配置与日志**：`/data/adb/android_agent/`（`config.json` 权限 `0600`、`agentd.log` 自动轮转 2MB）
- **运行时 PID 目录（`0700`，位于 `/data/adb` 受限区）**：`/data/adb/android_agent/run/`（`watchdog.pid`、`agentd.pid`，开机/启停均严格核验 `/proc/$PID/cmdline` 并自动清理陈旧 PID 文件，消除 `/dev/android_agent` 存在性痕迹）
- **临时运行文件（私有缓存，熔断/空闲自动清除）**：`/data/local/tmp/.ag_cache/helper.jar`（目录 `0700`、文件 `0600`，属主 `2000:2000` `shell:shell`，仅在微内核活跃时按需释放，游戏熔断或 3 分钟空闲退出后立即删除整个缓存目录）

---

## 2. 阶段 1 真机局域网实测结果（`192.168.100.100:8765`）

实测环境：Xiaomi 13（`2211133C`）、Android 16（API 36）、HyperOS `OS3.0.310.0.WMCCNXM`、KernelSU `3.3.1`。

| 能力项 | 实测结果 | 性能与细节 |
| :--- | :--- | :--- |
| **Bearer Token 强制鉴权** | 通过 | 无 Token / 错误 Token 返回 `401`；正确 Token 返回 `200` |
| **MCP Streamable HTTP** | 通过 | `POST /mcp` 支持 `2025-03-26` + `Mcp-Session-Id`，共暴露 16 个工具 |
| **本地截屏缩放压缩** | 通过 | `screencap` 原始 RGBA 本地缩放为 `720x1600` JPEG(q=70)，手机耗时 **`598ms`**（局域网端到端 **`637ms`**），单图仅 **`135 KB`**（较 10.4MB 原始数据压缩 ~77 倍） |
| **Android 16 中文剪贴板** | 通过 | `app_process64` + `AgentClip.smali` (`uid=2000`) 写入并读回 `"你好小米13澎湃OS-MCP中文测试🚀"` 一次无损通过 |
| **应用拉起与触控输入** | 通过 | 通过 `/system/bin/sh /system/bin/monkey` 拉起应用，触控/按键直调 `/system/bin/cmd input` |
| **Edge 浏览器 CDP** | 通过 | Microsoft Edge (`com.microsoft.emmx`, `EdgA/157`) 暴露 `@chrome_devtools_remote`：`cdp_status` **`21ms`**、`cdp_list_targets` **`15ms`**、`Runtime.evaluate` **`24~31ms`** |
| **防风控暂停/恢复** | 通过 | 手动暂停拦截所有操作工具；`com.tencent.tmgp.sgame`（王者荣耀）进前台自动关闭 8765 监听，退出 3 秒后自动恢复 |

---

## 2.5 三层混合引擎（`v0.3.0-hybrid-engine` / `hybrid-layered-v2`）与真机实测

在 `Xiaomi 13`（Android 16 API 36 / HyperOS 3.0）实机上的分阶段实测表现：

1. **感知层（懒加载 `app_process64 uid=2000` 微内核）**
   - 通过 `stdin/stdout` JSONL 管道驱动 `AgentKernel`，连接 `UiAutomation` 跳过 `waitForIdle` 并设最大深度（36）/节点上限（1200）防卡死；平时 0 内存，连续 **3 分钟无调用自动销毁**，王者荣耀（`com.tencent.tmgp.sgame`）进前台**秒杀微内核**。
   - **新增 3 个核心工具（工具总数增至 `23` 个）**：
     - `phone_ui_tree`：从内存直读控件树并提纯压缩（过滤不可见节点、密码脱敏、卡片合并、边界去重），控制在 **300~500 Token** 内并缓存 `ID -> bounds`。
     - `phone_tap_node`：根据 `[id]` 计算中心点并叠加有限随机偏移，走 `evdev` 拟人点击。
     - `phone_scroll_node`：在指定可滑动容器范围内执行拟人弧线滑动。
   - **真机实测**：稳态 `phone_ui_tree` 内部耗时 **`4~14 ms`**（美团首页 176 原始节点压缩为 39 节点 / `~440 tok`，较原生 `uiautomator dump` 6.3~12.3s 提速数百倍）；复用管道的 `phone_clipboard_get` 仅 **`26.2 ms`**。

2. **触控层（纯 Go `/dev/input/eventX` 物理拟人触控）**
   - 自动扫描 `/dev/input/event*` 的 `EVIOCGID` / Sysfs / `EVIOCGPROP` / `EVIOCGABS`，精准剔除蓝牙 `uhid` 外设，自动锁定小米 13 物理 SPI 触摸屏 **`/dev/input/event7`（`fts`，`0..10799 × 0..23999` 十倍亚像素量程，Type B 协议）**。
   - 点击随机按压 **`30~80ms`**（实测抓包 `57.06ms`），滑动使用 **二次贝塞尔微弧度 + Minimum-Jerk 变速**；写入全程互斥锁并用 `defer` 兜底补发抬起释放事件（`ABS_MT_TRACKING_ID=-1`、`BTN_TOUCH=0`、`SYN_REPORT=0`），绝不卡死屏幕。
   - **安全降级**：仅竖屏（`ROTATION_0`）启用，检测到横屏或设备写入异常时自动无缝回退 `/system/bin/cmd input`。

3. **兜底层（视觉与 Shell 保底）**
   - 遇到自绘页面/游戏/WebView（控件树为空）或微内核异常时，自动回退 `raw screencap` 720p JPEG（或 `cdp_*`）与 Shell 命令。

---

## 2.6 实时流式架构与零 Hook 超级岛（`v0.3.3-streaming`）

为解决移动网页“发送后卡住、切走看不见进度”的痛点，`v0.3.3-streaming` 在零外部依赖下补齐了全链路流式体验：

1. **服务端实时 SSE 与断线重放（Run Replay）**
   - `internal/brain/llm.go`：原生支持 OpenAI 与 Gemini 双协议 `stream: true` 增量文本与流式 `tool_calls` 组装；若上游不支持流式或返回异常，自动无感回退非流式请求。
   - `internal/brain/agent.go`：为 ReAct 多步循环新增结构化 `RunEvent` 事件流（`run_start` / `thinking` / `tool_start` / `tool_end` / `text_delta` / `needs_confirmation` / `done` / `cancelled` / `error`）与双环形内存缓冲（保留最近 128 个结构化步骤事件与 384 个文本增量）。
   - `internal/mcp/server.go`：新增 `POST /api/v1/chat/stream`（支持标准 SSE 与 KSU Bridge 异步模式 `async: true`）、`GET /api/v1/chat/events?run_id=...&after_seq=...` 断线事件重放，以及 `POST /api/v1/chat/cancel` 运行中一键急停。

2. **DSH Go 风格 `/ui` & KernelSU WebUI**
   - 发送后即时弹出带绿色呼吸灯的实时步骤时间线（`正在调用 …`），支持折叠/展开已完成步骤、打字机流式回复、发送键切换红色 **停止 (`■`)** 急停，并在浏览器切后台/恢复（`visibilitychange` / `online` / 页面刷新）时自动通过 `sessionStorage` + `/api/v1/chat/events` 补齐全部进度，严格保持原有本机无外链 CSP 策略。

3. **零 Hook HyperOS 3.0 超级岛焦点通知（`island.go` + `AgentKernel.smali`）**
   - 不修改 `system_server` / `SystemUI`、不依赖 LSPosed Hook，直接利用常驻 `AgentKernel`（`uid=2000`）向 HyperOS 3.0 发送含 `miui.focus.enable` / `miui.focus.ticker` / `miui.focus.param`（`param_island`）的高权重静默焦点通知（实测 **5ms** 亮起顶部超级岛胶囊）：
   - 空闲零打扰；执行期间 `350ms` 节流刷新；完成/取消 3 秒自动收起；触及 `SOUL.md` 付款/确认红线时保留提醒胶囊；王者荣耀进前台熔断时优先秒消超级岛再终止微内核。

---

## 2.7 `v0.3.4` 可靠性、功耗与安全加固摘要

在真机长期运行遥测与全链路代码审计后，`v0.3.4` 重点完成了以下修复与加固：

1. **MCP Server 与安全加固（`internal/mcp/server.go`）**
   - **横屏坐标精准映射**：`phone_screenshot` 缓存最近一次截屏的 `(OrigWidth, OrigHeight, Width, Height)`，`resolveCoords()` 分别计算 `scaleX` / `scaleY`，修复横屏 `coord_space="image"` 点击/滑动偏移。
   - **严格 Origin / Host 与动态 Token 校验**：拒绝空 `Host` 及未绑定外部 IP；`Origin` 严格校验 `scheme://host[:port]` 与请求 `Host` 一致（不再放行任意回环端口）；Bearer Token 改为动态读取 `cfgMu` + `sha256` 常量时间比对。
   - **会话与协议卫生**：MCP JSON-RPC 批处理同样校验 `Mcp-Session-Id`，自动回收超过 2 小时闲置会话；`/api/v1/chat/stream` 在响应头后立即刷新 `: connected\n\n`；`/api/v1/schedule` 严格校验 `HH:MM` 格式与 `window_end > window_start`。
   - **任务串行化与隐私脱敏**：MCP `run_daily_all` / `run_ms_rewards` / `run_jd_daily` 统一经由 `JobManager` 串行执行并支持 UI 追踪/取消；内置 LLM `ToolSpecs()` 隐藏 `agent_pause`/`agent_resume` 防自锁；`ExecuteTool` 与 `/api/v1/chat/history` 对剪贴板和输入敏感值进行脱敏。

2. **WebUI / KernelSU WebUI 体验与稳定性（`ui.html` / `ksu-module/webroot/index.html`）**
   - **修复重启/未知 `run_id` 永久卡死**：`replayOnce()` 遇到 `not_found`、HTTP 404 或 `(!active && !finished)` 时立即解除锁定并清理本地 `active_run_id`，不再无限 450ms 轮询；支持 `accumulated_text` 文本恢复与 `tool_start` 清理中间流文本。
   - **首次鉴权与省电轮询**：提取 `bootstrapAuthedUI()`，首次保存 Token 后立即加载历史/计划并启动轮询；按 `run_id` 去重避免刷新重复渲染气泡；页面隐藏（`document.hidden`）时暂停轮询，切回前台（`visibilitychange`）立即刷新。
   - **交互与配置保护**：`saveSchedule()` 编辑主计划时完整保留 `config.schedule.entries` 中的其他条目；弹窗内新增内联错误/提示条；`needs_confirmation` 确认横幅新增 **`[我已处理，继续]` / `[截屏核对]` / `[放弃任务]`** 快捷操作按钮（零外链，完全兼容本地 CSP）。

3. **端侧引擎、大脑与工作流协同加固**
   - 包含闲置 CPU/子进程减负（`guard.go` / `shell.go`）、CDP 冻结检测与跨 Socket 防误路由（`cdp.go`）、LLM 流式错误透传与并行工具调用兼容（`llm.go` / `agent.go`）、定时任务锁屏检查与多端设备互斥租约（`scheduler.go` / `workflows.go`）。

---

## 2.8 `v0.3.5` Anti-ACE 隐蔽加固摘要

针对 `com.tencent.tmgp.sgame`（王者荣耀 / ACE）实机全链路可观测性审计，`v0.3.5` 将 `android-agent` 在游戏期间的暴露面压缩至零：

1. **存活且未冻结即熔断 + 零 30s 亮屏竞态（`internal/guard/guard.go`）**
   - `AndroidForegroundDetector.DetectGuardedForeground` 只要检测到受保护游戏存在任意存活且未被 cgroup/freezer 冻结的进程（`!isPIDFrozen(pid)`），立即返回熔断而无需等待前台焦点或 fork `dumpsys`——下拉通知栏或临时切微信/小窗期间持续关闭 `127.0.0.1:8765` 并阻断 Cron 任务，待游戏退出或被 HyperOS 冻结后再按 `resumeGrace` 恢复。
   - `Guard.Run()` 在息屏期间改为每 `1s` 执行零 fork 轻量级 `screenChecker()`（sysfs 读取约 `50µs`），息屏状态仍按 `30s` 节流完整扫描，但一旦亮屏（`false -> true`）立即触发 `CheckOnce()` 并切回活跃轮询间隔，彻底消除解锁秒开游戏时的 30s 窗口。
   - 修复 `findRunningPackages` 中 `UID < 10000` 且 `cmdline` 暂为空的新生进程被永久写入 `knownSafePIDs` 的竞态，改为纳入 `volatilePIDs` 持续复检。

2. **私有化 `0700/0600` 微内核 JAR + 熔断/空闲自动删除 + PID 目录移出 `/dev`**
   - 将微内核 JAR 迁移至 `/data/local/tmp/.ag_cache/helper.jar`（目录 `0700`、文件 `0600`、`2000:2000` `shell:shell`，兼容自动迁移旧配置并清理遗留 `/data/local/tmp/android_agent_cliphelper.jar`），普通应用（`untrusted_app_30`）执行 `stat`/`read` 均返回 `EACCES`。
   - `StopMicroKernel`（`StopForGuard` 游戏熔断与 3 分钟空闲退出）自动删除 `helper.jar` 及 `.ag_cache` 目录，游戏与空闲期间 `/data/local/tmp` 零 JAR 残留。
   - `ksu-module/{service,action,uninstall}.sh` 将 PID 目录从 `/dev/android_agent` 迁移至 `/data/adb/android_agent/run`（`0700`），并自动清理旧 `/dev/android_agent`；针对持久化 PID 目录在启动/查询/卸载全路径强制校验 `watchdog.pid` 与 `agentd.pid` 对应的 `/proc/$PID/cmdline`，确保重启后陈旧 PID 文件绝不会造成假存活或卡死。

3. **去指纹化 `127.0.0.1:8765` 响应头（`internal/mcp/server.go`）**
   - 移除 `401 Unauthorized` 响应中 `WWW-Authenticate` 头的 `realm="android-agent"` 标识字符串（仅返回标准 `Bearer` / `Bearer error="invalid_token"`）。

---

## 2.9 `v0.4.0` 60fps 四栏移动控制台、长轮询减负与超级岛升级摘要

1. **零卡顿通信与高价值端点/复合工具（`internal/mcp/server.go`）**
   - **长轮询事件重放**：`GET /api/v1/chat/events?run_id=...&after_seq=...&wait_ms=8000`（上限 `12000ms`）在无新事件时静默挂起订阅，首个事件到达后合并 `35ms` 批量返回增量并支持游戏熔断即时唤醒，使 KernelSU WebUI 减少 **95%** Root Shell fork 并实现 ~35ms 丝滑响应。
   - **1-RTT 秒开启动**：新增 `GET /api/v1/bootstrap` 一次返回 `{status, jobs, saved_turns, history, llm, schedule, memory_meta}`，消除串行启动请求。
   - **画面远控、连通测速与超级岛预览**：新增 `GET /api/v1/screen/preview?with_tree=1`（并发抓取 720p JPEG + 前台包名 + 提纯控件树节点）、`POST /api/v1/screen/action`（支持图像坐标点触、节点点击、按键与滑动，受防风控与锁屏门禁保护）、`POST /api/v1/llm/test`（12s 超时连通性与 RTT 测速）、`POST /api/v1/island/test` 与 `POST /api/v1/schedule/run`。
   - **新增复合工具（总计 `28` 个 MCP 工具）**：新增 `phone_open_deeplink` 与 `phone_tap_and_type`（点击目标控件/坐标 + 等待 `350ms` + 输入文本并自动脱敏记录），显著降低多步 ReAct 往返延迟。

2. **HyperOS 3 暗黑玻璃四栏 `/ui` 与 KernelSU WebUI（`ui.html` / `webroot/index.html`）**
   - **60fps 前端渲染引擎**：纯内联 SVG 图标（零廉价 Emoji、零外链资源，严格兼容本地 CSP）、`requestAnimationFrame` 帧合并增量流式更新、90px 智能滚动锁与 `↓ 回到底部` 悬浮胶囊、KSU 内一键 `#token=` 一次性移交系统浏览器极速模式（加载后自动 `history.replaceState` 抹除 URL Hash）。
   - **四大功能视图**：
     - **助手**：6 大高频场景卡片、可折叠步骤耗时追踪卡片、流式 Markdown 与人工核对确认卡（一键跳转实时画面）。
     - **画面**：720p 实时屏幕预览、点触水波纹远程点击、控件树可视化边框叠层、虚拟硬件导航栏与可点击控件树检查器。
     - **任务**：0-Token 工作流控制台（实时进度条 + 阶段提示 + 滚动日志 + 一键急停）与多规则定时调度器（同时支持内置工作流与 `task_type: "prompt"` 自然语言 AI 定时任务，支持单条开关、立即试跑、编辑与删除）。
     - **系统**：澎湃灵动岛五态实时预览工作台、多预设大模型配置与 RTT 测速、带字节预算条与未保存提示的 Markdown 记忆工作台，以及设备遥测与守护一键暂停/恢复。

---

## 2.10 `v0.4.1` Codex 风格单步自观测循环、滑动折叠与防卡死接力升级摘要

1. **单步动作 + 自动界面回传（`internal/mcp/server.go` `ExecuteTool` + `agent.go` `SystemPrompt`）**
   - 内置大脑调用任一界面变更工具（`phone_tap`、`phone_tap_node`、`phone_long_press`、`phone_swipe`、`phone_scroll_node`、`phone_keyevent`、`phone_input_text`、`phone_tap_and_type`、`phone_launch_app`、`phone_open_deeplink`）成功后，除非显式传入 `observe: false`，自动等待页面稳定（常规触控/按键/输入 `350ms`、拉起应用/跳转深链 `900ms`，全程响应 `ctx.Done()` 与游戏风控门禁）并抓取提纯控件树（`MaxTokens: 450`），在返回结果末尾追加 `[操作后最新界面]` 及最新缓存的 `[id]` 编号，将「点击 + 抓树」两轮模型调用压缩为单轮。

2. **80 步高安全上限 + 滑动窗口上下文折叠 + 灵动岛自适应展示（`config.go`、`agent.go`、`island.go`）**
   - 默认 `MaxSteps` 从 `20` 提升至 **`80`**；`island.go` 在高上限模式（`Max >= 40`）下自动切换为清爽的 `"第 N 步 · 推理"` / `"第 N 步 · <动作>"` 文案与平滑渐近进度条（告别突兀的 `"3/80"`）。
   - `compactInFlightHistory` 升级为精确滑动窗口：仅保留最近 **2** 份完整 UI 树（`phone_ui_tree` 输出或变更工具内的 `[操作后最新界面]` 区块）与最近 **1** 张截屏 `ImageJPEG`，更早的历史界面自动折叠为单行 `[历史界面已折叠，请以最新界面为准]`（完整保留动作结果 JSON 头部），使第 50 步与第 3 步拥有同等轻量的上下文开销。

3. **周期 1/2/3 循环检测 + 界面指纹无变化防卡死（`internal/brain/agent.go`）**
   - 同时追踪最近工具调用签名（精准识别 `A→A→A` 周期 1、`A→B→A→B` 周期 2、`A→B→C→A→B→C` 周期 3 循环）与操作后 UI 树指纹（`extractUITreeFingerprint` 哈希 `App:` 与 `[id]` 节点行，识别连续 3 次变更操作页面毫无反应）。
   - 触发后自动注入 `[系统防卡死提示]` 引导关闭遮挡弹窗、换向滚动或调用 `phone_screenshot` 视觉兜底；若连续 **6** 步仍无界面进展则提前跳出循环进入优雅总结，绝不空耗 80 轮。

4. **无工具流式检查点汇报 + 一键接力操作栏（`agent.go`、`ui.html`、`webroot/index.html`）**
   - 到达步数上限或连续无进展提前退出时，自动发起一次 `tools = nil` 的流式总结调用（实时推送 `text_delta`，异常时由 `ToolTrace` 结构化兜底），向用户清晰汇报已完成步骤、当前停留页面与下一步建议，并在 `RunEvent` / `SavedTurn` / `ChatTurnResult` 标记 `checkpoint_paused: true`。
   - `/ui` 与 KernelSU WebUI 在助手气泡下方渲染靛青渐变接力操作栏，提供 **`[继续执行剩余步骤]`**（一键续跑未竟任务）与 **`[查看实时画面]`**（直达实时远控页人工排障）。

---

## 3. 常用运维命令

### 读取当前手机端 Bearer Token 并通过 `adb forward` 访问（宿主机执行）

> [!IMPORTANT]
> 自 **`v0.3.2-hardened`** 起，`agentd` 默认仅监听 **`127.0.0.1:8765`**（防止外出连接公共 Wi-Fi 时局域网明文暴露）。电脑端调试请先执行 `adb forward tcp:8765 tcp:8765`。

```bash
adb -s 192.168.100.100:5555 forward tcp:8765 tcp:8765
export AGENT_HOST="http://127.0.0.1:8765"
export AGENT_TOKEN="$(adb -s 192.168.100.100:5555 shell "su -c 'cat /data/adb/android_agent/config.json'" \
  | python3 -c 'import sys,json; print(json.load(sys.stdin)["bearer_token"])')"
```

### 常用 `curl` 接口检查与控制

```bash
# 1. 健康检查 & 查看屏幕、风控、Edge CDP 状态
curl -sS -H "Authorization: Bearer $AGENT_TOKEN" "$AGENT_HOST/health" | python3 -m json.tool

# 2. 手动暂停 Agent（防风控 / 临时接管手机）
curl -sS -X POST -H "Authorization: Bearer $AGENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"reason":"manual maintenance"}' \
  "$AGENT_HOST/api/v1/pause"

# 3. 恢复 Agent
curl -sS -X POST -H "Authorization: Bearer $AGENT_TOKEN" "$AGENT_HOST/api/v1/resume"

# 4. 测试 MCP initialize 握手
curl -sS -i -H "Authorization: Bearer $AGENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}' \
  "$AGENT_HOST/mcp"

# 5. 通过 MCP 查询 Edge CDP 标签页（免 Session 也可直接测试）
curl -sS -H "Authorization: Bearer $AGENT_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"cdp_list_targets","arguments":{}}}' \
  "$AGENT_HOST/mcp" | python3 -m json.tool
```

### 手机端进程、日志与启停管理（通过 ADB Root）

```bash
# 查看运行状态与最新日志
adb -s 192.168.100.100:5555 shell "su -c 'ps -ef | grep agentd | grep -v grep; tail -n 30 /data/adb/android_agent/agentd.log'"

# 停止守护进程（不重启手机）
adb -s 192.168.100.100:5555 shell "su -c 'touch /data/adb/android_agent/stop; kill \$(cat /data/adb/android_agent/run/agentd.pid 2>/dev/null) 2>/dev/null || true'"

# 重新拉起守护进程
adb -s 192.168.100.100:5555 shell "su -c 'rm -f /data/adb/android_agent/stop; nohup sh /data/adb/modules/android_agent/service.sh </dev/null >/dev/null 2>&1 &'"

# 轮换 Token：直接编辑 /data/adb/android_agent/config.json 中的 bearer_token 后重启 agentd 即可
```

### Antigravity MCP 配置（配合 `adb forward tcp:8765 tcp:8765`）

```json
{
  "mcpServers": {
    "xiaomi13-agent": {
      "url": "http://127.0.0.1:8765/mcp",
      "headers": {
        "Authorization": "Bearer <替换为 /data/adb/android_agent/config.json 中的 bearer_token>"
      }
    }
  }
}
```

---

## 4. 本地重新编译与测试

宿主机使用 `/root/router-dashboard/go/bin/go`（Go 1.27.1）与 `/usr/share/java/smali.jar` 构建：

```bash
/root/android-agent/scripts/build.sh
```

脚本会依次执行：汇编 `cliphelper.jar` -> 运行 `go test -v -race ./...` -> 交叉编译静态 `linux/arm64` 二进制至 `ksu-module/bin/agentd` -> 验证静态 ELF。

---

## 5. 未来开启阶段 2（LSPosed `@agent_sys`）的预留说明

当前架构已为阶段 2 做好解耦，以后若需要内存截屏（绕过 `FLAG_SECURE`）、`InputManagerService` 内存注入或微信 `RemoteInput` 静默回复，只需：

1. **宿主机补齐构建链**：安装最小 Android SDK（`cmdline-tools`、`platforms;android-36`、`d8`、`apksigner`，JDK 21 已就绪）。
2. **提取目标系统签名**：从小米 13（Android 16 / API 36 / HyperOS 3.0）只读分析 `services.jar` / `framework.jar` 的 `ScreenCapture`、`InputManagerService`、`NotificationManagerService` 精确签名。
3. **新增 LSPosed 极薄模块**：仅勾选系统框架 `android`，独立后台线程监听 `LocalServerSocket("agent_sys")`，全链路 `try-catch` 防阻塞/软重启。
4. **Go 端无缝切换**：新增实现 [backend.go](file:///root/android-agent/internal/backend/backend.go) 中 `DeviceBackend` 接口的 `@agent_sys` 客户端，保持 MCP Server 与风控逻辑零改动。
