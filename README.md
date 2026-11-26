# Android AI Agent (`android-agent`)

小米 13（`fuxi` / Android 16 / API 36 / HyperOS 3.0 / KernelSU 3.3.1）原生低延迟 AI Agent 底座。

- **当前状态**：**`v0.3.0-hybrid-engine` 已实现三层混合引擎（`hybrid-layered-v2`：4ms 内存 UI 树 + 纯 Go `/dev/input/event7` 拟人触控 + 720p 视觉兜底）与手机端自闭环大脑（0-Token 原生日常引擎 + 双协议 LLM API 大脑 + KernelSU/移动 Web 控制台 `/ui`）**。
- **架构亮点**：零外部 Go 依赖静态单文件，内置标准 **MCP Streamable HTTP (`2025-03-26`)** 服务端（`23` 个工具）、懒加载 `app_process64` 微内核（3 分钟空闲自动销毁 + 王者荣耀前台熔断）、OpenAI/Gemini 双协议大脑与移动 PWA 控制台。

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
│   ├── brain/                         # 本机内置 LLM 大脑（OpenAI/Gemini 双协议 + 进程内 ToolExecutor + JobManager）
│   │   ├── llm.go
│   │   ├── agent.go
│   │   └── brain_test.go
│   ├── webui/                         # 手机端暗黑模式移动 Web 控制台（//go:embed ui.html，支持添加到桌面）
│   │   ├── webui.go
│   │   └── ui.html
│   └── mcp/
│       ├── server.go                  # 标准 MCP Streamable HTTP (2025-03-26) + /ui + /api/v1/jobs/* + /api/v1/chat + /api/v1/llm/config
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
- **运行时 PID 目录（tmpfs，重启自动清空）**：`/dev/android_agent/`（`watchdog.pid`、`agentd.pid`，叠加 `/proc/$PID/cmdline` 防重入校验）
- **临时运行文件**：`/data/local/tmp/android_agent_cliphelper.jar`（首次操作剪贴板时自动释放，供 `uid=2000` `app_process64` 读取）

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

## 3. 常用运维命令

### 读取当前手机端 Bearer Token（宿主机执行）

```bash
export AGENT_HOST="http://192.168.100.100:8765"
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
adb -s 192.168.100.100:5555 shell "su -c 'touch /data/adb/android_agent/stop; kill \$(cat /dev/android_agent/agentd.pid 2>/dev/null) 2>/dev/null || true'"

# 重新拉起守护进程
adb -s 192.168.100.100:5555 shell "su -c 'rm -f /data/adb/android_agent/stop; nohup sh /data/adb/modules/android_agent/service.sh </dev/null >/dev/null 2>&1 &'"

# 轮换 Token：直接编辑 /data/adb/android_agent/config.json 中的 bearer_token 后重启 agentd 即可
```

### Antigravity MCP 配置（`~/.gemini/config/mcp_config.json`）

```json
{
  "mcpServers": {
    "xiaomi13-agent": {
      "url": "http://192.168.100.100:8765/mcp",
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
