# Android AI Agent (`android-agent`)

小米 13（`fuxi` / Android 16 / API 36 / HyperOS 3.0 / KernelSU 3.3.1）原生低延迟 AI Agent 底座。

- **当前状态**：**阶段 1（Go ARM64 Daemon + KernelSU 模块 + Edge/Chrome CDP + Android 16 中文剪贴板）已上线稳定运行**，满足日常低延迟操作需求；为保持系统稳定与降低游戏风控，**暂缓阶段 2（LSPosed `system_server` Hook）**。
- **架构亮点**：零外部 Go 依赖静态单文件（`7.0 MB`），内置标准 **MCP Streamable HTTP (`2025-03-26`)** 服务端（`0.0.0.0:8765/mcp`），全路由强制 `Authorization: Bearer <token>` 鉴权。

---

## 1. 项目目录结构

### 宿主机代码（`/root/android-agent`）

```text
/root/android-agent/
├── cmd/agentd/main.go                 # 守护进程入口（配置加载、信号处理、CDP 探测、风控调度）
├── internal/
│   ├── config/config.go               # config.json 解析、环境变量覆盖、首次启动自动生成 64 位随机 Token
│   ├── backend/
│   │   ├── backend.go                 # DeviceBackend 抽象接口（为阶段 2 @agent_sys 预留无缝替换点）
│   │   ├── shell.go                   # 阶段 1 实现：raw screencap、cmd input/window、sh monkey、app_process64 剪贴板
│   │   └── cliphelper.jar             # 内嵌的 1.9KB Dalvik 剪贴板助手（//go:embed）
│   ├── imgutil/
│   │   ├── compress.go                # 原始 RGBA / PNG 解码 + 双线性等比缩放(720px) + JPEG(q=70) 压缩
│   │   └── compress_test.go
│   ├── guard/
│   │   ├── guard.go                   # 游戏防风控：/proc 零子进程预检，王者荣耀进前台直接关闭 8765 监听
│   │   └── guard_test.go
│   ├── cdp/
│   │   ├── cdp.go                     # 直连抽象 Unix Socket (@chrome_devtools_remote + 自动探测 *_devtools_remote*)
│   │   └── cdp_test.go
│   └── mcp/
│       ├── server.go                  # 标准 MCP Streamable HTTP (2025-03-26) + Bearer 鉴权 + 16 个工具 + REST 控制接口
│       └── server_test.go
├── cliphelper/AgentClip.smali         # 适配 Android 16 IClipboard 签名的剪贴板助手 Smali 源码
├── ksu-module/                        # KernelSU 模块打包目录
│   ├── module.prop
│   ├── service.sh                     # 开机自启与守护进程保活脚本
│   ├── action.sh                      # KSU 管理器一键查看/拉起脚本
│   ├── uninstall.sh                   # 卸载清理脚本
│   ├── config.json.example
│   └── bin/agentd                     # linux/arm64 静态编译产物（.gitignore 忽略，运行 build.sh 生成）
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
