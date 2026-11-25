---
name: ms-rewards-daily
description: >-
  小米 13 微软积分（Microsoft Rewards）每日全自动任务执行技能。
  当用户要求“刷微软积分”、“做今日 Rewards 任务”、“跑一下 Edge 和 Bing 积分”、“执行每日积分任务”时激活此技能。
  通过手机端 android-agent MCP Server（192.168.100.100:8765）联动 Edge CDP 与 Bing App 触控，
  自动完成 Dashboard Daily Set 每日三件套、日常任务、在必应上浏览、必应每日搜索（100/100）、
  Bing App 每日签入与快速滑底阅读以赚取（30/30）。
---

# Microsoft Rewards 每日积分自动化技能（小米 13）

通过小米 13 上运行的 KernelSU Go Daemon（`http://192.168.100.100:8765/mcp`）完成每日全部微软积分任务。核心自动化脚本位于：[rewards_runner.py](./scripts/rewards_runner.py)。

## 安全红线与积分规则

1. **人机验证 / 登录拦截即停**：脚本在每次页面加载后会检测 CAPTCHA /「Verify you are human」/「人机验证」；一旦触发或要求登录，**立即中止**并交由用户在手机上亲自完成，绝不尝试绕过。
2. **尊重游戏风控守护**：若 `phone_status` 显示 `guard.state != "active"`（如王者荣耀前台或手动暂停），**立即中止**。
3. **不同问答类型的计分规则（重要）**：
   - **每日投票（Daily Poll, +10）**：无对错，任选一项即得 10 分（脚本自动完成）。
   - **极速问答（Supersonic / Warpspeed / Lightspeed Quiz, +30）**：答错不扣分，但需点中正确选项过关拿满 30 分（脚本自动检测并点击“下一题”推进，拿满即止）。
   - **二选一测验（This or That, +50）**：**每答对 1 题 +5 分，答错 0 分且不可重选！** 脚本检测到 `This or That` 时绝不盲点，会列入 `interactive_tasks`，由 Agent 审题后使用 `edge-quiz-step` 精准点击正确答案。
4. **阅读新闻触发积分要点（0 截屏 WebView CDP 直控）**：Bing App「阅读以赚取（30/30）」**仅支持纯文字新闻**，视频卡片（`/vi-` / 含 `▶` 图标）不计分！若已将 `必应 (com.microsoft.bing)` 加入 `Agent WebView Debug` 作用域，脚本 `bing-read` 自动通过 `@webview_devtools_remote_<pid>` 递归穿透 MSN Shadow DOM，只挑 `a.heading[href*="/ar-"]` 纯文字新闻点击并滑底（**彻底 0 截屏、0 视频误触**）；未开启时自动回退到本地 `PIL+numpy` 像素识图过滤。
5. **0 截屏面板读写与签入**：`bing-check` 与 `bing-checkin` 在 Bing WebView CDP 就绪时直接通过 20ms DOM 读取积分、`阅读以赚取` 进度并点击 `.check-in-card` 签入（先自动左区下拉刷新防缓存旧数据）；也可直接用仓库一键串联脚本 `python3 /root/android-agent/scripts/daily_all.py` 在 ~4.5 分钟内跑完微软 + 京东全套。
5. **避开右上角悬浮窗**：用户在手机上常开右上角悬浮窗（`720x1600` 图坐标 `x > 490, y < 420`），Bing App 触控与滑动统一使用中左区域（`x = 260`），且忽略 `com.server.auditor.ssh.client` 焦点。
6. **不擅自激活第三方促销**（Discord Nitro、盗贼之海等），**绝不执行 `reboot` 或重启 `system_server`**。

---

## 标准执行流程

### Step 0：前置健康与电量检查

```bash
adb connect 192.168.100.100:5555 >/dev/null 2>&1 || true
python3 /root/.agents/skills/ms-rewards-daily/scripts/rewards_runner.py status
```

- 若 `guard.state != "active"`：向用户说明原因并停止。
- 若手机电量 `< 10%`：提醒用户留意充电。

### Step 1：Edge 浏览器 CDP 一键全清（Dashboard Daily Set + Earn 日常 + 在必应上浏览 + 必应搜索 100/100）

```bash
python3 /root/.agents/skills/ms-rewards-daily/scripts/rewards_runner.py edge-all
```

脚本自动依次完成：
1. **`rewards.bing.com/dashboard` 每日三件套（Daily Set）**：点击链接卡（+10）、真实 CDP 坐标穿透 Shadow DOM 完成每日投票（+10）与 3 题极速问答（+30）；若遇到计对错的 `This or That (+50)` 会暂停并输出到 `interactive_tasks` 交由 Agent 审题作答。
2. **`rewards.bing.com/earn` 日常任务**：自动点击未完成的日常任务卡片。
3. **「在必应上浏览」**：动态识别所有 `已激活` 卡片并按高优先级领域词典精准搜索（邮轮/航班/保险/贷款/套餐等优先于通用词）。
4. **每日必应搜索（`100/100`）**：按日期轮换多样化搜索词、7.5s 自然间隔补满 `100/100`（遇 15 分钟冷却自动安全停止）。

> 若 `interactive_tasks` 中出现 `This or That` 等计对错测验，使用 `edge-quiz-inspect` 查看题目和选项，再由 Agent 判断正确答案并用 `edge-quiz-click --text "<正确选项>"` 逐题作答。

### Step 2：Bing App 每日签入与极速滑底阅读（`30/30`）

1. **检查 Bing App Rewards 面板**：

```bash
python3 /root/.agents/skills/ms-rewards-daily/scripts/rewards_runner.py bing-check
```

用 `read_file` 查看 `/tmp/ms-rewards-shots/bing_check_1.jpg` 与 `bing_check_2.jpg`：
- 若 **`签入`** 未打绿勾（显示金色数字圆圈），运行：
  ```bash
  python3 /root/.agents/skills/ms-rewards-daily/scripts/rewards_runner.py bing-checkin
  ```
- 若 **`阅读以赚取`** 未满 `30 ✓`，运行极速滑底阅读（默认每篇打开后快速连滑 3 次直达文章最底部，单篇仅 ~4.5s）：
  ```bash
  python3 /root/.agents/skills/ms-rewards-daily/scripts/rewards_runner.py bing-read --count 11
  ```
- 复核输出截图 `bing_read_after_1.jpg` 与 `bing_read_after_2.jpg`；若偶遇广告/视频差最后 1~2 篇，使用 `bing-feed-shot --swipes 3` 截图选定非广告图文新闻坐标，再执行 `bing-tap-article --x <x> --y <y>`（同样自动滑到底部触发 `+3`）精准补齐。

### Step 3：向用户汇报战果

整理精简表格汇报：
- **初始可用积分 -> 最终可用积分（净增 `+N`）**
- 分项完成状态：`Daily Set 三件套`、`日常任务`、`在必应上浏览`、`必应搜索 (100/100)`、`Bing 每日签入`、`Bing 阅读以赚取 (30/30)`。
