---
name: ms-rewards-daily
description: >-
  小米 13 微软积分（Microsoft Rewards）每日全自动任务执行技能。
  当用户要求“刷微软积分”、“做今日 Rewards 任务”、“跑一下 Edge 和 Bing 积分”、“执行每日积分任务”时激活此技能。
  通过手机端 android-agent MCP Server（192.168.100.100:8765）联动 Edge CDP 与 Bing App 触控，
  自动完成日常任务、在必应上浏览、必应每日搜索（100/100）、Bing App 每日签到与阅读以赚取（30/30）。
---

# Microsoft Rewards 每日积分自动化技能（小米 13）

通过小米 13 上运行的 KernelSU Go Daemon（`http://192.168.100.100:8765/mcp`）完成每日全部微软积分任务。核心自动化脚本位于：[rewards_runner.py](./scripts/rewards_runner.py)。

## 安全红线（执行前必读）

1. **人机验证 / 登录拦截即停**：脚本在每次页面加载后会检测 CAPTCHA /「Verify you are human」/「人机验证」；一旦触发或要求登录，**立即中止**并交由用户在手机上亲自完成，绝不尝试绕过。
2. **尊重游戏风控守护**：若 `phone_status` 显示 `guard.state != "active"`（如王者荣耀前台或手动暂停），**立即中止**，不得强行恢复。
3. **不擅自激活第三方促销**：默认**不点击激活** Discord Nitro、盗贼之海等需开启倒计时窗口的第三方奖品任务，除非用户明确指定。
4. **避开右上角悬浮窗**：用户在手机上常开右上角悬浮窗（`720x1600` 图坐标 `x > 490, y < 420`），Bing App 触控与滑动统一使用中左区域（`x = 260`）。
5. **只操作任务，绝不执行 `reboot` 或重启 `system_server`**。

---

## 标准执行流程

### Step 0：前置健康与电量检查

运行状态检查，确认 `android-agent` 在线、`guard.state == "active"`、查看手机电量与当前积分：

```bash
python3 /root/.agents/skills/ms-rewards-daily/scripts/rewards_runner.py status
```

- 若 `guard.state != "active"`：向用户说明原因并停止。
- 若手机电量 `< 10%`：提醒用户留意充电（若 `<= 2%` 建议先充再跑）。

### Step 1：Edge 浏览器 CDP 积分任务（`rewards.bing.com/earn`）

一键执行 Edge 侧全部可自动化任务（日常任务卡片 + `在必应上浏览` 已激活主题搜索 + 补满 `必应搜索 100/100`）：

```bash
python3 /root/.agents/skills/ms-rewards-daily/scripts/rewards_runner.py edge-all
```

脚本会输出结构化 JSON 报告，包含：
- `before` / `after` 积分与 `必应搜索 xx/100` 对比
- `daily_clicked`：自动点击完成的日常任务卡片
- `interactive_tasks`：如当日出现「极速问答 / 每周测验 / Daily Poll」等交互式题目卡片，脚本会列出标题与链接，由 Agent 随后用 `cdp_command` 打开页面并逐题点击完成
- `explore_done`：动态识别并完成的 `已激活`「在必应上浏览」任务
- `search_status`：`completed`（已满 `100/100`）或 `cooldown_detected`（若账号触发 15 分钟冷却会自动安全停止并如实汇报）

### Step 2：Bing App 每日签到与「阅读以赚取（30/30）」

1. **检查 Bing App Rewards 面板**：

```bash
python3 /root/.agents/skills/ms-rewards-daily/scripts/rewards_runner.py bing-check
```

用 `read_file` 查看输出的两张截图（默认 `/tmp/ms-rewards-shots/bing_check_1.jpg` 与 `bing_check_2.jpg`）：
- 检查 **连续打卡 -> `签入`** 是否已带绿色 `✓`；若未签到，运行：
  ```bash
  python3 /root/.agents/skills/ms-rewards-daily/scripts/rewards_runner.py bing-checkin
  ```
- 检查 **`阅读以赚取`**：若已是 `30 ✓`，直接跳到 Step 3；否则记下当前进度（例如 `0/30` 需 10 篇，建议 `--count 12` 留出广告卡片余量）。

2. **自动刷满「阅读以赚取」**：

```bash
python3 /root/.agents/skills/ms-rewards-daily/scripts/rewards_runner.py bing-read --count 12
```

脚本完成后会保存验证截图 `bing_read_after_1.jpg` 与 `bing_read_after_2.jpg`，用 `read_file` 查看：
- 若已达到 `30 ✓` 与 `今日积分 180/180`：完成！
- 若因遇到视频/广告卡片差最后 1~2 篇（如 `24/30` 或 `27/30`）：
  1. 运行 `bing-feed-shot --swipes 4` 截取新鲜资讯流，用 `read_file` 目视选出非广告 MSN/BBC 等新闻卡片的图坐标 `(x, y)`；
  2. 运行 `bing-tap-article --x <x> --y <y>` 精准阅读并复核 `bing-check`，直至 `30 ✓`。

### Step 3：向用户汇报战果

整理精简表格汇报：
- **初始可用积分 -> 最终可用积分（净增 `+N`）**
- 分项完成状态：`日常任务`、`在必应上浏览`、`必应搜索 (xx/100)`、`Bing 每日签入`、`Bing 阅读以赚取 (xx/30)`
- 若存在冷却暂停、交互测验未清、验证码拦截或低电量情况，明确提示。

---

## 故障快速排查

- **`android-agent` 无法连接**：检查手机 Wi-Fi 是否仍在 `192.168.100.100`，或执行 `adb connect 192.168.100.100:5555 && adb -s 192.168.100.100:5555 shell "su -c 'ps -ef | grep agentd | grep -v grep'"`。
- **Edge CDP 不可用**：Edge 未在后台运行，脚本的 `edge-all` 会自动先调用 `phone_launch_app("com.microsoft.emmx")` 拉起 Edge 并等待 `@chrome_devtools_remote` 就绪。
- **Token 读取优先级**：脚本自动依次尝试环境变量 `AGENT_TOKEN` -> `~/.gemini/config/mcp_config.json` -> ADB 读取手机 `/data/adb/android_agent/config.json`。
