---
name: jd-daily
description: >-
  小米 13 京东（JD）极速日常任务与自动优质评价技能（WebView CDP 20ms DOM 直控 + 原生评价页联动）。
  当用户要求“刷京东农场”、“做京东每日任务”、“浇一下东东农场”、“领京豆签到”、“京东自动评价”、“把待评价商品自动好评领京豆”时激活此技能。
  通过手机端 android-agent MCP Server（192.168.100.100:8765）直连京东 WebView DevTools 抽象 Socket
  （@webview_devtools_remote_<pid>，依赖极薄 LSPosed 模块 Agent WebView Debug），
  自动完成东东农场每日签到、收树上水滴、6 秒安全浏览任务、阶梯浇水任务、领京豆签到，以及评价中心待评价订单的定制化百字 5 星优质文字好评（不含盗图/私图）。
---

# 京东每日极速任务技能（小米 13 · WebView CDP）

通过小米 13 上运行的 `android-agent`（`http://192.168.100.100:8765/mcp`）直连京东 WebView CDP（`@webview_devtools_remote_<pid>`），实现零截图、20ms 级 DOM 直控。核心执行脚本：[jd_runner.py](./scripts/jd_runner.py)。

## 前置依赖与安全红线

1. **前置模块**：手机已安装极薄零权限 LSPosed 模块 `Agent WebView Debug`（`com.agent.webviewdebug`，仅勾选 `京东`，不碰 `system_server`），京东打开 H5 页面后自动暴露 `@webview_devtools_remote_<pid>`。
2. **安全白名单（严格防黑号 / 防误操作）**：
   - **只做**免费且低风险的常规项：东东农场每日签到、树上水滴收集、纯 6 秒安全浏览任务（如 `精选活动推荐`、`限时领京豆浏览`）、每日浇水 20 次及领奖、领京豆每日签到。
   - **绝不做**：下单/支付/开通会员/金融借贷、下载第三方 App（如快手极速版）、添加桌面小组件、关注陌生店铺/频道、拉新邀请。
3. **风控与验证码红线**：
   - 若 `phone_status` 显示 `guard.state != "active"`（如王者荣耀前台或手动暂停），**立即中止**。
   - 遇到任何滑块验证、人机验证或登录弹窗，**立即中止**并交由用户亲自处理。
   - 绝不执行 `reboot` 或重启 `system_server`。

---

## 标准执行流程

### Step 0：检查服务与京东 WebView Socket

```bash
adb connect 192.168.100.100:5555 >/dev/null 2>&1 || true
python3 /root/.agents/skills/jd-daily/scripts/jd_runner.py status
```

- 确认 `guard.state == "active"`；若未检测到 `@webview_devtools_remote_<pid>`，脚本的 `farm-all` 会自动拉起京东并通过中文剪贴板搜索「东东农场」唤醒 WebView。

### Step 1：一键执行东东农场全套日常（`farm-all`）

```bash
python3 /root/.agents/skills/jd-daily/scripts/jd_runner.py farm-all
```

脚本自动完成（全程 WebView CDP DOM 直控，零大模型看图等待）：
1. 拉起京东并定位「东东农场」WebView Target（如未打开则中文剪贴板搜「东东农场」进入）。
2. 自动关闭进场弹窗、点击树上 `10水滴` 气泡。
3. 进入「签到」页完成每日签到（`+20g`）。
4. 打开「领水滴」任务抽屉，按安全白名单完成所有纯 6 秒浏览任务（每次停留 ~12s 防“浏览时长不足”）并自动点 `去领奖`。
5. **只完成所有「每日累计浇水」水滴任务、不盲目耗光余水、临近收获留给用户**：默认 `--water-mode tasks`，自动检测任务抽屉中的 `每日累计浇水X次(cur/target)`（含多阶段阶梯浇水任务），精准浇够对应次数并领取全部水滴奖励后立即停手，把多余水滴持续囤住；若监测到距离最终收获 `<= 5%`（`nearHarvest`），自动暂停并把**最后一次收获浇水留给用户亲自操作**（如你明确想一口气浇光余水，也可随时传 `--water-mode all`）。
6. 结束前再次检查任务抽屉领完所有 `去领奖`，输出 JSON 战报（水滴余额、成长进度、中奖红包记录）。

> 如只需把当前余水全部浇完并领取全部浇水奖励，也可单独调用 `farm-water`；如需领京豆签到，可调用 `bean-signin`（直跳 `bean.m.jd.com/bean/signIndex.action`）。

### Step 1B：一键自动完成待评价商品优质 5 星文字好评（`review-list` / `review-all`）

```bash
# 查看评价中心当前待评价商品列表与已获京豆统计
python3 /root/.agents/skills/jd-daily/scripts/jd_runner.py review-list

# 批量提交优质 5 星文字好评（每条 90~130 字贴合品类特征 + 自动勾选正面标签/尺码印象，默认绝不盗图、不自动上传相册私图）
python3 /root/.agents/skills/jd-daily/scripts/jd_runner.py review-all --max-count 30 --delay 2.5
```

- 可选传入 `--reviews-json <path>` 为本次特定商品提供逐条定制评语映射（`{"商品名关键字": "定制百字评语"}`），未命中时自动按商品大类（鞋服、母婴、厨具日用、数码家电、食品、延保服务等）生成差异化真实口吻评语。
- 自动规避手机右上角 SSH 悬浮窗触摸区域（`dumpsys input` 实时探测 `touchableRegion`，使用 `发布` 按钮顶部安全区点击），遇滑块/人机验证立即中止。

### Step 2：汇报战果

整理精简战报返回给用户：
- 东东农场当前阶段（发芽 / 开花 / 结果进度）与最新水滴余额
- 完成的签到、浏览任务次数、浇水次数及领到的无门槛红包/升级水滴
- 领京豆签到结果（如执行了 `bean-signin`）
- 自动评价提交条数、跳过条数及「已写评价 / 已获京豆」前后对比（如执行了 `review-all`）
