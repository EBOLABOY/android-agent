#!/usr/bin/env python3
"""
JD daily automation runner (东东农场 + 领京豆) for Xiaomi 13 via android-agent WebView CDP.
Pure Python 3 standard library (zero external dependencies).
"""

import argparse
import json
import os
import re
import subprocess
import sys
import time
import urllib.parse
import urllib.request
from pathlib import Path

DEFAULT_HOST = os.environ.get("AGENT_HOST", "http://192.168.100.100:8765")
DEFAULT_ADB = os.environ.get("AGENT_ADB_SERIAL", "192.168.100.100:5555")
JD_PKG = "com.jingdong.app.mall"

CAPTCHA_RE = re.compile(
    r"安全验证|滑块验证|拖动滑块|人机验证|verify you are human|captcha|请完成验证",
    re.IGNORECASE,
)

# Tasks explicitly forbidden to touch (third-party installs, widgets, follow spam, paid orders, mini-games)
FORBIDDEN_TASK_RE = re.compile(
    r"快手|百度|极速版|小组件|关注|邀请|好友|下单|购买|支付|开通PLUS|开通会员|金融|借款|白条|信用卡|助力|通过1关|小游戏|旅行一次",
    re.IGNORECASE,
)

# Safe browse tasks allowed for automatic 12s view + claim
SAFE_BROWSE_RE = re.compile(
    r"浏览\d+秒|读\d+s|精选活动推荐|限时领|逛一逛|逛PLUS|逛家电|逛看病|抢爆品|京东直播",
    re.IGNORECASE,
)


def resolve_token(adb_serial: str) -> str:
    env_tok = os.environ.get("AGENT_TOKEN", "").strip()
    if env_tok:
        return env_tok
    mcp_cfg = Path.home() / ".gemini" / "config" / "mcp_config.json"
    if mcp_cfg.exists():
        try:
            data = json.loads(mcp_cfg.read_text(encoding="utf-8"))
            for srv in (data.get("mcpServers") or {}).values():
                url = str(srv.get("url") or "")
                if "8765" in url or "192.168.100.100" in url:
                    auth = (srv.get("headers") or {}).get("Authorization", "")
                    if auth.startswith("Bearer "):
                        tok = auth.removeprefix("Bearer ").strip()
                        if tok and not tok.startswith("<"):
                            return tok
        except Exception:
            pass
    out = subprocess.check_output(
        ["adb", "-s", adb_serial, "shell", "su -c 'cat /data/adb/android_agent/config.json'"],
        text=True,
        timeout=10,
    )
    return json.loads(out)["bearer_token"]


class MCPClient:
    def __init__(self, base_url: str, token: str):
        self.base_url = base_url.rstrip("/")
        self.token = token
        self.sid = None
        self.rpc_id = 0
        self._init()

    def _post(self, payload: dict, timeout: int = 25) -> dict:
        headers = {
            "Content-Type": "application/json",
            "Authorization": f"Bearer {self.token}",
        }
        if self.sid:
            headers["Mcp-Session-Id"] = self.sid
        req = urllib.request.Request(
            self.base_url + "/mcp",
            data=json.dumps(payload).encode("utf-8"),
            headers=headers,
            method="POST",
        )
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            if not self.sid:
                self.sid = resp.headers.get("Mcp-Session-Id") or resp.headers.get("mcp-session-id")
            return json.loads(resp.read().decode("utf-8"))

    def _init(self):
        self.rpc_id += 1
        self._post(
            {
                "jsonrpc": "2.0",
                "id": self.rpc_id,
                "method": "initialize",
                "params": {"protocolVersion": "2025-03-26"},
            }
        )

    def call(self, name: str, args: dict = None) -> dict:
        self.rpc_id += 1
        resp = self._post(
            {
                "jsonrpc": "2.0",
                "id": self.rpc_id,
                "method": "tools/call",
                "params": {"name": name, "arguments": args or {}},
            }
        )
        res = resp.get("result") or {}
        if res.get("isError"):
            msg = (res.get("content") or [{}])[0].get("text", "unknown error")
            raise RuntimeError(f"MCP tool {name} failed: {msg}")
        return res

    def call_json(self, name: str, args: dict = None) -> dict:
        return json.loads(self.call(name, args)["content"][0]["text"])

    def cdp(self, socket: str, target_id: str, method: str, params: dict = None) -> dict:
        args = {"socket": socket, "method": method}
        if target_id:
            args["target_id"] = target_id
        if params is not None:
            args["params"] = params
        return self.call_json("cdp_command", args)["result"]

    def eval_js(self, socket: str, target_id: str, expr: str):
        r = self.cdp(socket, target_id, "Runtime.evaluate", {"expression": expr, "returnByValue": True})
        if "exceptionDetails" in r:
            raise RuntimeError(f"JS exception: {json.dumps(r['exceptionDetails'], ensure_ascii=False)}")
        return r.get("result", {}).get("value")


def require_guard_active(mc: MCPClient) -> dict:
    st = mc.call_json("phone_status")
    guard = st.get("guard") or {}
    if guard.get("state") != "active":
        raise RuntimeError(f"Agent guard is not active (state={guard.get('state')}); aborting safely.")
    return st


def find_farm_target(mc: MCPClient):
    try:
        res = mc.call_json("cdp_list_targets", {"socket": JD_PKG})
    except Exception:
        return None
    if not res.get("available"):
        return None
    for t in res.get("targets") or []:
        if t.get("type") == "page" and ("东东农场" in (t.get("title") or "") or "42HV4J3Q87B2xFQMJk81PCc1mEs3" in (t.get("url") or "")):
            return t
    return None


def open_jd_h5_deeplink(adb_serial: str, url: str) -> bool:
    """Jump directly to a JD Babel/H5 activity via JD's openapp.jdmobile:// deep link."""
    try:
        params = urllib.parse.quote(json.dumps({"category": "jump", "des": "m", "url": url}, separators=(",", ":")))
        uri = f"openapp.jdmobile://virtual?params={params}"
        subprocess.run(
            ["adb", "-s", adb_serial, "shell", "am", "start", "-a", "android.intent.action.VIEW", "-d", uri, JD_PKG],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            timeout=10,
        )
        return True
    except Exception:
        return False


def ensure_farm_open(mc: MCPClient, adb_serial: str = DEFAULT_ADB) -> dict:
    """Bring JD foreground and ensure 东东农场 WebView target is live."""
    mc.call("phone_launch_app", {"package": JD_PKG})
    time.sleep(1.8)
    t = find_farm_target(mc)
    if t:
        return t

    # Primary: direct JD deep link to 东东农场 (works regardless of active bottom tab!)
    if open_jd_h5_deeplink(adb_serial, "https://pro.m.jd.com/mall/active/42HV4J3Q87B2xFQMJk81PCc1mEs3/index.html"):
        for _ in range(8):
            time.sleep(1.0)
            t = find_farm_target(mc)
            if t:
                return t

    # Fallback: return to MainFrameActivity, tap real 首页 tab (155, 1515; 东东 mascot is at far-left 0..68), then search
    for _ in range(5):
        fg = mc.call_json("phone_foreground_app")
        if fg.get("package") == JD_PKG and "MainFrameActivity" in (fg.get("activity") or ""):
            break
        if fg.get("package") not in (JD_PKG, "com.server.auditor.ssh.client", ""):
            mc.call("phone_launch_app", {"package": JD_PKG})
            time.sleep(1.5)
        else:
            mc.call("phone_keyevent", {"keycode": "BACK"})
            time.sleep(1.2)
        t = find_farm_target(mc)
        if t:
            return t

    mc.call("phone_tap", {"x": 155, "y": 1515, "coord_space": "image"})
    time.sleep(0.8)
    mc.call("phone_tap", {"x": 155, "y": 1515, "coord_space": "image"})
    time.sleep(1.0)

    mc.call("phone_tap", {"x": 300, "y": 200, "coord_space": "image"})
    time.sleep(1.2)
    mc.call("phone_input_text", {"text": "东东农场", "method": "clipboard", "submit": True})
    for _ in range(12):
        time.sleep(1.0)
        t = find_farm_target(mc)
        if t:
            return t
    raise RuntimeError(
        "东东农场 WebView target not found. Ensure LSPosed module 'Agent WebView Debug' is enabled for 京东 and JD was restarted."
    )


def check_captcha(text: str):
    if CAPTCHA_RE.search(text or ""):
        raise RuntimeError("CAPTCHA / slider verification detected! Stopping immediately per safety policy.")


def dismiss_popups(mc: MCPClient, tid: str) -> list:
    res = mc.eval_js(
        JD_PKG,
        tid,
        """
        (() => {
          const txt = (document.body?.innerText || "");
          const actions = [];
          // Record any red packet win amount before closing
          const redMatch = txt.match(/¥\\s*\\d+(?:\\.\\d+)?/);
          for (const el of Array.from(document.querySelectorAll("div, span, button"))) {
            const t = (el.innerText || "").trim();
            const r = el.getBoundingClientRect();
            if (/^(开心收下|收下了|我知道了|好的|继续浇水|明日再来|明天再来)$/.test(t) && r.width > 30 && r.height > 18) {
              el.click();
              actions.push("btn:" + t);
              return {actions, redPacket: redMatch ? redMatch[0] : null, snippet: txt.slice(0, 300)};
            }
          }
          if (/恭喜中奖|无门槛红包|买买买|升级福利|水滴奖励\\+10/.test(txt)) {
            const closes = Array.from(document.querySelectorAll("[class*=close_icon], [class*=close], [class*=Close], img[src*=close]"))
              .filter(e => {
                const r = e.getBoundingClientRect();
                return r.width > 12 && r.width < 90 && r.height > 12 && r.height < 90 && r.top > 60;
              });
            if (closes.length > 0) {
              closes[closes.length - 1].click();
              actions.push("close_modal");
            }
          }
          return {actions, redPacket: redMatch ? redMatch[0] : null, snippet: txt.slice(0, 300)};
        })()
        """,
    )
    check_captcha((res or {}).get("snippet", ""))
    return res or {"actions": []}


def read_farm_summary(mc: MCPClient, tid: str) -> dict:
    res = mc.eval_js(
        JD_PKG,
        tid,
        """
        (() => {
          const txt = (document.body?.innerText || "");
          const wm = txt.match(/\\b(\\d+)g\\b/);
          const water = wm ? wm[0] : null;
          const waterNum = wm ? parseInt(wm[1], 10) : null;
          const stage = (txt.match(/再浇([\\d.]+)%([^\\n!！]*)/) || [null, null, null]);
          const stageText = stage[0] ? stage[0].trim() : null;
          const remainPct = stage[1] ? parseFloat(stage[1]) : null;
          const stageTarget = stage[2] ? stage[2].trim() : "";
          const nearHarvest = (remainPct !== null && remainPct <= 5.0 && /收获|成熟/.test(stageTarget));
          const needPrize = /请选择奖品|档位奖励示意图/.test(txt);
          const matured = /恭喜成熟|水果已成熟|去兑换奖品|立即兑换奖品|可兑换奖品/.test(txt);
          return {water, waterNum, stage: stageText, remainPct, nearHarvest, needPrize, matured, snippet: txt.slice(0, 400)};
        })()
        """,
    )
    return res or {}


def read_watering_task_status(mc: MCPClient, tid: str) -> dict:
    """Inspect the currently open task drawer for any '每日累计浇水X次(cur/target)' task."""
    return mc.eval_js(
        JD_PKG,
        tid,
        """
        (() => {
          const txt = (document.body?.innerText || "");
          const m = txt.match(/每日累计浇水(\\d+)次\\s*[（(](\\d+)\\/(\\d+)[）)]/);
          if (!m) return {found: false};
          return {found: true, label: m[0], cur: parseInt(m[2], 10), target: parseInt(m[3], 10)};
        })()
        """,
    ) or {"found": False}


def open_task_drawer(mc: MCPClient, tid: str) -> bool:
    res = mc.eval_js(
        JD_PKG,
        tid,
        """
        (() => {
          if ((document.body?.innerText || "").includes("做任务 领水滴")) return "already_open";
          const wrap = document.querySelector(".bottombar_wrap");
          if (wrap && wrap.children.length >= 3) {
            wrap.children[2].click();
            return "clicked_bar";
          }
          return null;
        })()
        """,
    )
    if res:
        time.sleep(0.7)
        return True
    return False


def close_task_drawer(mc: MCPClient, tid: str):
    mc.eval_js(
        JD_PKG,
        tid,
        """
        (() => {
          if (!(document.body?.innerText || "").includes("做任务 领水滴")) return;
          const closes = Array.from(document.querySelectorAll("[class*=close], [class*=Close], img[src*=close]"))
            .filter(e => { const r = e.getBoundingClientRect(); return r.width > 12 && r.width < 80 && r.top > 60 && r.top < 360; });
          if (closes.length > 0) closes[closes.length - 1].click();
        })()
        """,
    )
    time.sleep(0.5)


def claim_all_drawer_rewards(mc: MCPClient, tid: str) -> int:
    claimed = mc.eval_js(
        JD_PKG,
        tid,
        """
        (() => {
          let n = 0;
          const clickedParents = new Set();
          for (const el of Array.from(document.querySelectorAll("div, span, button"))) {
            const t = (el.innerText || "").trim();
            const r = el.getBoundingClientRect();
            if (/^(去领奖|领奖励|领取)$/.test(t) && r.width > 20 && r.height > 15) {
              const p = el.parentElement;
              if (p && clickedParents.has(p)) continue;
              if (p) clickedParents.add(p);
              el.scrollIntoView({block: "center"});
              el.click();
              n++;
            }
          }
          return n;
        })()
        """,
    )
    if claimed:
        time.sleep(0.7)
    return int(claimed or 0)


def do_farm_signin(mc: MCPClient, tid: str) -> dict:
    # Click 1st icon in .bottombar_wrap (签到)
    opened = mc.eval_js(
        JD_PKG,
        tid,
        """
        (() => {
          const wrap = document.querySelector(".bottombar_wrap");
          if (wrap && wrap.children.length >= 1) { wrap.children[0].click(); return true; }
          return false;
        })()
        """,
    )
    if not opened:
        return {"ok": False, "note": "signin icon not found"}
    time.sleep(2.5)
    # Check if signin opened a new WebView page or inline modal
    targets = mc.call_json("cdp_list_targets", {"socket": JD_PKG}).get("targets") or []
    sign_page = next((t for t in targets if t.get("type") == "page" and t["id"] != tid), None)
    result = {"ok": False, "note": "already signed or native"}
    if sign_page:
        sid = sign_page["id"]
        clicked = mc.eval_js(
            JD_PKG,
            sid,
            """
            (() => {
              const txt = document.body?.innerText || "";
              if (/已签到|明天再来/.test(txt)) return "already_signed";
              const btn = Array.from(document.querySelectorAll("div, span, button")).find(e => {
                const t = (e.innerText || "").trim();
                const r = e.getBoundingClientRect();
                return /^(点击签到领水滴|立即签到|点击签到|签到领水滴)$/.test(t) && r.width > 40;
              });
              if (btn) { btn.click(); return "clicked:" + btn.innerText.trim(); }
              return null;
            })()
            """,
        )
        result = {"ok": bool(clicked), "detail": clicked}
        time.sleep(2.0)
        mc.call("phone_keyevent", {"keycode": "BACK"})
        time.sleep(1.5)
    else:
        # Fallback: tap known sign-in button center if inline
        mc.call("phone_tap", {"x": 360, "y": 668, "coord_space": "image"})
        time.sleep(2.0)
        mc.call("phone_keyevent", {"keycode": "BACK"})
        time.sleep(1.5)
        result = {"ok": True, "detail": "tapped_fallback"}
    return result


def return_to_farm_activity(mc: MCPClient) -> str:
    """Press BACK until JD's 东东农场 BabelActivity is actually back in the foreground,
    dismissing retention modals (e.g. 看病买药 / 直播 exit popups) that eat hardware BACK.
    """
    for attempt in range(6):
        fg = mc.call_json("phone_foreground_app")
        pkg = fg.get("package") or ""
        act = fg.get("activity") or ""
        if pkg == JD_PKG and "BabelActivity" in act:
            t = find_farm_target(mc)
            if t:
                return t["id"]
        if pkg not in (JD_PKG, "com.server.auditor.ssh.client", ""):
            mc.call("phone_launch_app", {"package": JD_PKG})
            time.sleep(1.5)
        else:
            if attempt in (1, 3):
                # Dismiss common JD center-bottom retention modal 'X' (e.g. 看病买药 5京豆弹窗)
                mc.call("phone_tap", {"x": 360, "y": 1142, "coord_space": "image"})
                time.sleep(0.8)
            mc.call("phone_keyevent", {"keycode": "BACK"})
            time.sleep(1.0)
            if attempt in (2, 4):
                mc.call("phone_tap", {"x": 45, "y": 124, "coord_space": "image"})
                time.sleep(1.0)
    return ensure_farm_open(mc)["id"]


def wait_farm_ready(mc: MCPClient, tid: str, timeout: float = 14.0) -> dict:
    """Poll 东东农场 WebView DOM until React SPA hydrates (waterNum or stage visible)."""
    deadline = time.time() + timeout
    last = {}
    while time.time() < deadline:
        last = read_farm_summary(mc, tid)
        snip = last.get("snippet") or ""
        if last.get("waterNum") is not None or last.get("needPrize") or last.get("matured") or re.search(r"再浇|浇水|浇5次", snip):
            return last
        time.sleep(1.0)
    return last


def do_safe_browse_tasks(mc: MCPClient, tid: str, max_runs: int = 24, browse_sec: float = 8.5) -> dict:
    completed = []
    if not open_task_drawer(mc, tid):
        return {"completed": completed, "claims": 0}
    claims = claim_all_drawer_rewards(mc, tid)
    tried_counts = {}
    stalled_progress = {}

    for _ in range(max_runs):
        # Always claim completed step rewards first so (cur/target) advances cleanly
        claims += claim_all_drawer_rewards(mc, tid)

        # Scan ALL task cards in DOM (even below fold!) and find first SAFE browse task
        cand = mc.eval_js(
            JD_PKG,
            tid,
            """
            (() => {
              const seen = new Set();
              const rows = [];
              for (const btn of Array.from(document.querySelectorAll("div, span, button"))) {
                const bt = (btn.innerText || "").trim();
                const br = btn.getBoundingClientRect();
                if (!/^(去完成|逛一逛)$/.test(bt) || br.width < 30 || br.height < 18) continue;
                let row = btn.parentElement;
                for (let k = 0; k < 4 && row; k++) {
                  if ((row.innerText || "").length > 15 && row.getBoundingClientRect().width > 240) break;
                  row = row.parentElement;
                }
                const rtxt = (row ? row.innerText : bt).replace(/\\s+/g, " ").trim();
                if (seen.has(rtxt)) continue;
                seen.add(rtxt);
                rows.push({btnText: bt, rowText: rtxt.slice(0, 100), y: Math.round(br.top)});
              }
              return rows;
            })()
            """,
        ) or []

        chosen = None
        for r in cand:
            rtxt = r["rowText"]
            if FORBIDDEN_TASK_RE.search(rtxt):
                continue
            if not SAFE_BROWSE_RE.search(rtxt):
                continue
            m_prog = re.search(r"[（(](\d+)/(\d+)[）)]", rtxt)
            cur_n = int(m_prog.group(1)) if m_prog else 0
            tgt_n = int(m_prog.group(2)) if m_prog else 1
            key = re.sub(r"[（(]\d+/\d+[）)]", "", rtxt)[:24]
            max_tries = (tgt_n + 6) if tgt_n > 1 else 2
            if tried_counts.get(key, 0) >= max_tries:
                continue
            # Also stop retrying if stuck at exact same cur_n 3 times in a row
            prev_cur, same_cnt = stalled_progress.get(key, (-1, 0))
            if cur_n == prev_cur and same_cnt >= 3:
                continue
            chosen = (r, key, cur_n)
            break

        if not chosen:
            break
        r, key, cur_n = chosen
        tried_counts[key] = tried_counts.get(key, 0) + 1
        prev_cur, same_cnt = stalled_progress.get(key, (-1, 0))
        stalled_progress[key] = (cur_n, (same_cnt + 1) if cur_n == prev_cur else 1)

        # Scroll into view and click the chosen row's button via DOM
        clicked = mc.eval_js(
            JD_PKG,
            tid,
            f"""
            (() => {{
              const want = {json.dumps(r["rowText"][:24])};
              for (const btn of Array.from(document.querySelectorAll("div, span, button"))) {{
                const bt = (btn.innerText || "").trim();
                const br = btn.getBoundingClientRect();
                if (!/^(去完成|逛一逛)$/.test(bt) || br.width < 30 || br.height < 18) continue;
                let row = btn.parentElement;
                for (let k = 0; k < 4 && row; k++) {{
                  if ((row.innerText || "").length > 15 && row.getBoundingClientRect().width > 240) break;
                  row = row.parentElement;
                }}
                if (row && (row.innerText || "").replace(/\\s+/g, " ").includes(want)) {{
                  btn.scrollIntoView({{block: "center"}});
                  btn.click();
                  return true;
                }}
              }}
              return false;
            }})()
            """,
        )
        if not clicked:
            break

        # Wait for JD 6s countdown with gentle scroll interactions (compact 8.5s total by default)
        time.sleep(2.0)
        mc.call("phone_swipe", {"x1": 360, "y1": 1100, "x2": 360, "y2": 680, "duration_ms": 220, "coord_space": "image"})
        time.sleep(3.0)
        mc.call("phone_swipe", {"x1": 360, "y1": 1000, "x2": 360, "y2": 720, "duration_ms": 200, "coord_space": "image"})
        time.sleep(max(1.2, browse_sec - 5.0))

        # Return to 东东农场 BabelActivity in foreground and refresh WebView target_id
        mc.call("phone_keyevent", {"keycode": "BACK"})
        time.sleep(1.0)
        tid = return_to_farm_activity(mc)

        open_task_drawer(mc, tid)
        claims += claim_all_drawer_rewards(mc, tid)
        completed.append(r["rowText"][:60])

    close_task_drawer(mc, tid)
    return {"completed": completed, "claims": claims, "tid": tid}


def do_watering(
    mc: MCPClient,
    tid: str,
    rounds: int = 30,
    mode: str = "tasks",
    keep_water: int = 0,
    allow_mature: bool = False,
) -> dict:
    """Water via 25ms WebView DOM clicks.
    - mode="tasks" (default): ONLY waters enough to complete all '每日累计浇水X次' milestone tasks
      in the task drawer and claim their water rewards, preserving surplus water for steady daily accumulation.
      Automatically pauses if the tree is about to harvest (<=5% left) so the user can do the final watering.
    - mode="all": pours all available water down to keep_water (still pausing before harvest unless allow_mature=True).
    - mode="fixed": clicks watering button up to `rounds` times.
    """
    watered = 0
    red_packets = []
    popups = []
    drawer_claims = 0
    stop_reason = "completed"
    stalled = 0
    min_threshold = max(10, keep_water)

    def _single_water_click():
        nonlocal watered
        res = mc.eval_js(
            JD_PKG,
            tid,
            """
            (() => {
              const btn = Array.from(document.querySelectorAll("div, span, button")).find(e => {
                const t = (e.innerText || "").trim();
                const r = e.getBoundingClientRect();
                return /^(浇5次|浇水|浇1次)$/.test(t) && r.width > 20 && r.top > 200;
              });
              if (!btn) return null;
              btn.click();
              return btn.innerText.trim();
            })()
            """,
        )
        if not res:
            return None
        watered += 1
        time.sleep(1.8)
        d2 = dismiss_popups(mc, tid)
        if d2.get("redPacket"):
            red_packets.append(d2["redPacket"])
        if d2.get("actions"):
            popups.extend(d2["actions"])
            time.sleep(0.5)
        return res

    if mode == "tasks":
        # Loop over chained watering tasks (e.g. 10次 -> 20次 -> 30次) until all watering tasks are claimed
        for _cycle in range(6):
            if open_task_drawer(mc, tid):
                drawer_claims += claim_all_drawer_rewards(mc, tid)
                wt = read_watering_task_status(mc, tid)
                close_task_drawer(mc, tid)
            else:
                stop_reason = "drawer_unavailable"
                break

            if not wt.get("found"):
                stop_reason = "all_watering_tasks_done"
                break
            rem = wt["target"] - wt["cur"]
            if rem <= 0:
                if open_task_drawer(mc, tid):
                    drawer_claims += claim_all_drawer_rewards(mc, tid)
                    close_task_drawer(mc, tid)
                stop_reason = "all_watering_tasks_done"
                break

            # Estimate clicks needed (5 waterings per '浇5次' click, capped by remaining rounds)
            need_clicks = min(max(1, (rem + 4) // 5), max(1, rounds - watered))
            stopped_early = False
            for _ in range(need_clicks):
                d = dismiss_popups(mc, tid)
                if d.get("redPacket"):
                    red_packets.append(d["redPacket"])
                if d.get("actions"):
                    popups.extend(d["actions"])
                    time.sleep(0.5)

                summary = read_farm_summary(mc, tid)
                if summary.get("matured"):
                    stop_reason = "tree_matured_ready_to_redeem"
                    stopped_early = True
                    break
                if not allow_mature and summary.get("nearHarvest"):
                    stop_reason = "paused_near_harvest_for_user"
                    stopped_early = True
                    break
                cur_w = summary.get("waterNum")
                if cur_w is not None and cur_w < 10:
                    stop_reason = "water_insufficient"
                    stopped_early = True
                    break

                if not _single_water_click():
                    stop_reason = "water_button_not_found"
                    stopped_early = True
                    break
            if stopped_early or watered >= rounds:
                break
        return {
            "mode": mode,
            "rounds_clicked": watered,
            "stop_reason": stop_reason,
            "drawer_claims": drawer_claims,
            "red_packets": red_packets,
            "popups_dismissed": popups,
        }

    # mode == "all" or "fixed"
    for idx in range(1, rounds + 1):
        d = dismiss_popups(mc, tid)
        if d.get("redPacket"):
            red_packets.append(d["redPacket"])
        if d.get("actions"):
            popups.extend(d["actions"])
            time.sleep(0.5)

        summary = read_farm_summary(mc, tid)
        if summary.get("matured"):
            stop_reason = "tree_matured_ready_to_redeem"
            break
        if not allow_mature and summary.get("nearHarvest"):
            stop_reason = "paused_near_harvest_for_user"
            break
        cur_water = summary.get("waterNum")
        if mode == "all" and cur_water is not None and cur_water < min_threshold:
            stop_reason = f"water_below_{min_threshold}g"
            break

        if not _single_water_click():
            stop_reason = "water_button_not_found"
            break

        after_w = read_farm_summary(mc, tid).get("waterNum")
        if cur_water is not None and after_w is not None and after_w >= cur_water:
            stalled += 1
            if stalled >= 4:
                stop_reason = "water_unchanged_or_daily_limit"
                break
        else:
            stalled = 0

        if mode == "all" and (idx % 8 == 0):
            if open_task_drawer(mc, tid):
                drawer_claims += claim_all_drawer_rewards(mc, tid)
                close_task_drawer(mc, tid)

    return {
        "mode": mode,
        "rounds_clicked": watered,
        "stop_reason": stop_reason,
        "drawer_claims": drawer_claims,
        "red_packets": red_packets,
        "popups_dismissed": popups,
    }


def cmd_status(mc: MCPClient, _args):
    st = mc.call_json("phone_status")
    socks = mc.call_json("cdp_list_sockets")
    out = {"phone_status": st, "cdp_sockets": socks.get("sockets", [])}
    t = find_farm_target(mc)
    if t:
        out["farm_target"] = {"id": t["id"], "title": t.get("title")}
        out["farm_summary"] = read_farm_summary(mc, t["id"])
    print(json.dumps(out, ensure_ascii=False, indent=2))


def cmd_farm_water(mc: MCPClient, args):
    require_guard_active(mc)
    t = ensure_farm_open(mc)
    tid = t["id"]
    before = read_farm_summary(mc, tid)
    wres = do_watering(
        mc,
        tid,
        rounds=args.max_rounds,
        mode=args.mode,
        keep_water=args.keep_water,
        allow_mature=args.allow_mature,
    )
    if open_task_drawer(mc, tid):
        wres["final_drawer_claims"] = claim_all_drawer_rewards(mc, tid)
        close_task_drawer(mc, tid)
    after = read_farm_summary(mc, tid)
    print(json.dumps({"before": before, "watering": wres, "after": after}, ensure_ascii=False, indent=2))


def cmd_farm_all(mc: MCPClient, args):
    require_guard_active(mc)
    tid = return_to_farm_activity(mc)
    before = wait_farm_ready(mc, tid, timeout=14.0)
    report = {"before": before}
    if report["before"].get("needPrize"):
        report["stopped_reason"] = "prize_selection_required"
        print(json.dumps(report, ensure_ascii=False, indent=2))
        return

    dismiss_popups(mc, tid)

    # 1. Collect tree floating 10g water bubble & left-side "10水滴(领)" entry if present
    bubble_res = mc.eval_js(
        JD_PKG,
        tid,
        """
        (() => {
          const hits = [];
          const b = document.querySelector(".FixedTaskIcon_fixed_task_img__388lV");
          if (b) { b.click(); hits.push("tree_bubble"); }
          const left10 = Array.from(document.querySelectorAll(".LeftTaskEntry_task_item__o3Mg-")).find(e => (e.innerText||"").includes("10水滴"));
          if (left10) { left10.click(); hits.push("left_10g"); }
          return hits;
        })()
        """,
    )
    report["bubbles_clicked"] = bubble_res or []
    time.sleep(1.0)
    dismiss_popups(mc, tid)

    # 2. Daily sign-in
    report["signin"] = do_farm_signin(mc, tid)
    tid = ensure_farm_open(mc)["id"]
    wait_farm_ready(mc, tid, timeout=10.0)
    dismiss_popups(mc, tid)

    # 3. Safe 6s browse tasks (accumulate continuous water drops)
    if not args.skip_browse:
        bres = do_safe_browse_tasks(mc, tid, max_runs=args.max_browse, browse_sec=args.browse_sec)
        tid = bres.pop("tid", tid)
        report["browse_tasks"] = bres

    # 4. Complete watering milestone tasks ONLY (preserving surplus water & leaving final harvest watering for user)
    tid = ensure_farm_open(mc)["id"]
    wait_farm_ready(mc, tid, timeout=10.0)
    report["watering"] = do_watering(
        mc,
        tid,
        rounds=args.max_water_rounds,
        mode=args.water_mode,
        keep_water=args.keep_water,
        allow_mature=args.allow_mature,
    )

    # 5. Final check of task drawer for any newly completed rewards
    if open_task_drawer(mc, tid):
        report["final_claims"] = claim_all_drawer_rewards(mc, tid)
        close_task_drawer(mc, tid)

    report["after"] = read_farm_summary(mc, tid)
    print(json.dumps(report, ensure_ascii=False, indent=2))


def find_bean_target(mc: MCPClient):
    try:
        res = mc.call_json("cdp_list_targets", {"socket": JD_PKG})
    except Exception:
        return None
    for t in res.get("targets") or []:
        if t.get("type") == "page" and "4E1fFrgX1eGtWTc5KBNFzXMrh6xn" in (t.get("url") or ""):
            return t
    return None


def cmd_bean_signin(mc: MCPClient, args):
    """Open JD 领京豆 (天天刮京豆 Babel activity) via native search (avoids broken bean.m.jd.com JFE 52)
    and trigger real tap on '立即刮卡'.
    """
    require_guard_active(mc)
    float_rect = get_floating_ssh_touch_rect(args.adb)
    t = find_bean_target(mc)
    if not t:
        if open_jd_h5_deeplink(args.adb, "https://pro.m.jd.com/mall/active/4E1fFrgX1eGtWTc5KBNFzXMrh6xn/index.html"):
            for _ in range(7):
                time.sleep(1.0)
                t = find_bean_target(mc)
                if t:
                    break
    if not t:
        for _ in range(5):
            fg = mc.call_json("phone_foreground_app")
            if fg.get("package") == JD_PKG and "MainFrameActivity" in (fg.get("activity") or ""):
                break
            mc.call("phone_keyevent", {"keycode": "BACK"})
            time.sleep(1.2)
        mc.call("phone_launch_app", {"package": JD_PKG})
        time.sleep(1.5)
        mc.call("phone_tap", {"x": 155, "y": 1515, "coord_space": "image"})
        time.sleep(0.8)
        mc.call("phone_tap", {"x": 155, "y": 1515, "coord_space": "image"})
        time.sleep(1.0)
        mc.call("phone_tap", {"x": 300, "y": 200, "coord_space": "image"})
        time.sleep(1.1)
        mc.call("phone_input_text", {"text": "领京豆", "method": "clipboard", "submit": True})
        time.sleep(3.2)

        # Find and tap red '进入' button safely avoiding floating SSH window
        xml = dump_ui_xml(args.adb)
        check_captcha(xml)
        nodes = parse_jd_nodes(xml)
        enter_nd = next((n for n in nodes if n["text"] in ("进入", "领88豆")), None)
        if enter_nd:
            x1, y1, x2, y2 = enter_nd["bounds"]
            cx, cy = (x1 + x2) // 2, (y1 + y2) // 2
            if not point_in_rect(cx, cy, float_rect):
                mc.call("phone_tap", {"x": cx, "y": cy, "coord_space": "screen"})
            else:
                mc.call("phone_tap", {"x": 625, "y": 305, "coord_space": "image"})
        else:
            mc.call("phone_tap", {"x": 625, "y": 305, "coord_space": "image"})
        for _ in range(8):
            time.sleep(1.0)
            t = find_bean_target(mc)
            if t:
                break

    if not t:
        raise RuntimeError("领京豆 WebView target (4E1fFrgX1eGtWTc5KBNFzXMrh6xn) not found.")

    tid = t["id"]
    time.sleep(1.5)
    info = mc.eval_js(
        JD_PKG,
        tid,
        """
        (() => {
          const txt = (document.body?.innerText || "");
          const btn = Array.from(document.querySelectorAll("div, span, button")).find(e => {
            const s = (e.innerText || "").trim();
            const r = e.getBoundingClientRect();
            return /^(立即刮卡|明日再来|今日已刮)$/.test(s) && r.width > 40 && r.height > 15;
          });
          let rect = null;
          if (btn) {
            const r = btn.getBoundingClientRect();
            rect = {text: btn.innerText.trim(), x: r.left + r.width / 2, y: r.top + r.height / 2, vw: window.innerWidth, vh: window.innerHeight};
          }
          const worth = (txt.match(/可抵[\\d.]+元/) || [null])[0];
          return {worth, rect, snippet: txt.slice(0, 400)};
        })()
        """,
    ) or {}
    check_captcha(info.get("snippet", ""))

    action = "no_scratch_button"
    rect = info.get("rect")
    if rect and rect.get("text") == "立即刮卡":
        # Convert CSS viewport coords -> 720x1600 image coords if vw available, else safe (360, 535)
        vw = rect.get("vw") or 393
        tx = int(round(rect["x"] * 720.0 / vw))
        ty = int(round(rect["y"] * 720.0 / vw))
        if not (200 <= ty <= 900):
            tx, ty = 360, 535
        mc.call("phone_tap", {"x": tx, "y": ty, "coord_space": "image"})
        action = f"tapped_scratch_card({tx},{ty})"
        time.sleep(2.5)
    elif rect:
        action = f"already_done:{rect.get('text')}"

    after_txt = mc.eval_js(JD_PKG, tid, "(document.body?.innerText || '').slice(0, 400)") or ""
    worth_after = (re.search(r"可抵[\d.]+元", after_txt) or [None])[0]
    print(json.dumps({"action": action, "worth_before": info.get("worth"), "worth_after": worth_after}, ensure_ascii=False, indent=2))


# ---------------------------------------------------------------------------
# JD Automatic High-Quality Product Reviews (评价中心 WebView + CommentFloorActivity)
# ---------------------------------------------------------------------------

REVIEW_CENTER_URL = "https://comment.m.jd.com/m-rate/rate-center?hideNavi=1&osPlant=jd-android"


def find_review_center_target(mc: MCPClient):
    try:
        res = mc.call_json("cdp_list_targets", {"socket": JD_PKG})
    except Exception:
        return None
    if not res.get("available"):
        return None
    for t in res.get("targets") or []:
        if t.get("type") == "page" and "rate-center" in (t.get("url") or ""):
            return t
    return None


def return_to_review_center(mc: MCPClient) -> str:
    """Press BACK until JD's rate-center WebActivity is in the foreground and return its WebView target_id."""
    for attempt in range(7):
        fg = mc.call_json("phone_foreground_app")
        pkg = fg.get("package") or ""
        act = fg.get("activity") or ""
        t = find_review_center_target(mc)
        if pkg == JD_PKG and "WebActivity" in act and t:
            time.sleep(0.8)
            return t["id"]
        if pkg not in (JD_PKG, "com.server.auditor.ssh.client", ""):
            mc.call("phone_launch_app", {"package": JD_PKG})
            time.sleep(1.5)
        else:
            mc.call("phone_keyevent", {"keycode": "BACK"})
            time.sleep(1.6)
            if attempt == 3:
                targets = mc.call_json("cdp_list_targets", {"socket": JD_PKG}).get("targets") or []
                page = next((x for x in targets if x.get("type") == "page"), None)
                if page:
                    mc.cdp(JD_PKG, page["id"], "Page.navigate", {"url": REVIEW_CENTER_URL})
                    time.sleep(3.2)
    t = find_review_center_target(mc)
    if t:
        return t["id"]
    raise RuntimeError(
        "评价中心 WebView target not found. Open 京东 -> 我的 -> 待评价 first, or ensure Agent WebView Debug is active."
    )


def reload_review_center(mc: MCPClient, tid: str) -> str:
    """Safely reload rate-center via CDP Page.reload (avoids eval_js reload race) and return fresh tid."""
    try:
        mc.cdp(JD_PKG, tid, "Page.reload", {"ignoreCache": False})
    except Exception:
        pass
    time.sleep(3.2)
    return return_to_review_center(mc)


def read_review_center_state(mc: MCPClient, tid: str) -> dict:
    last_err = None
    for attempt in range(3):
        try:
            res = mc.eval_js(
                JD_PKG,
                tid,
                """
                (() => {
                  const txt = document.body?.innerText || "";
                  const revM = txt.match(/(\\d+)\\s*(?:\\+\\d+\\s*)?已写评价/);
                  const beanM = txt.match(/(\\d+)\\s*已获京豆/);
                  const btns = Array.from(document.querySelectorAll("div, span, button")).filter(e => {
                    const s = (e.innerText || "").trim();
                    const r = e.getBoundingClientRect();
                    return s === "去评价" && r.width > 20 && r.width < 115 && r.height > 15 && r.height < 60;
                  });
                  const cards = [];
                  const seen = new Set();
                  for (const b of btns) {
                    let p = b.parentElement;
                    for (let k = 0; k < 8 && p; k++) {
                      const r = p.getBoundingClientRect();
                      const s = (p.innerText || "").trim();
                      if (r.height > 120 && r.height < 460 && s.length > 30 && !s.includes("已写评价")) break;
                      p = p.parentElement;
                    }
                    if (!p || (p.innerText || "").includes("已写评价")) continue;
                    const lines = (p.innerText || "").split("\\n").map(x => x.trim()).filter(Boolean);
                    if (!lines.length || lines[0].includes("限时激励")) continue;
                    if (seen.has(lines[0])) continue;
                    seen.add(lines[0]);
                    cards.push({title: lines[0], meta: lines.slice(1).join(" | ").slice(0, 140)});
                  }
                  return {
                    reviewed: revM ? parseInt(revM[1], 10) : null,
                    beans: beanM ? parseInt(beanM[1], 10) : null,
                    cards,
                    snippet: txt.slice(0, 300),
                  };
                })()
                """,
            )
            return res or {"cards": []}
        except Exception as e:
            last_err = e
            time.sleep(2.0)
            tid = return_to_review_center(mc)
    raise last_err


def click_review_card(mc: MCPClient, tid: str, target_title: str) -> bool:
    return bool(
        mc.eval_js(
            JD_PKG,
            tid,
            f"""
            (() => {{
              const want = {json.dumps(target_title)};
              const btns = Array.from(document.querySelectorAll("div, span, button")).filter(e => {{
                const s = (e.innerText || "").trim();
                const r = e.getBoundingClientRect();
                return s === "去评价" && r.width > 20 && r.width < 115 && r.height > 15 && r.height < 60;
              }});
              for (const b of btns) {{
                let p = b.parentElement;
                for (let k = 0; k < 8 && p; k++) {{
                  const r = p.getBoundingClientRect();
                  const s = (p.innerText || "").trim();
                  if (r.height > 120 && r.height < 460 && s.length > 30 && !s.includes("已写评价")) break;
                  p = p.parentElement;
                }}
                if (!p || (p.innerText || "").includes("已写评价")) continue;
                const lines = (p.innerText || "").split("\\n").map(x => x.trim()).filter(Boolean);
                if (lines.length && lines[0] === want) {{
                  b.scrollIntoView({{block: "center"}});
                  b.click();
                  return true;
                }}
              }}
              return false;
            }})()
            """,
        )
    )


def generate_quality_review(title: str, bespoke_map: dict, idx: int = 0) -> str:
    """Generate a tailored, authentic 90-130 Chinese char 5-star review for the product."""
    for key, text in (bespoke_map or {}).items():
        if key and key in title:
            return text

    # Clean short product label (strip bracketed promo noise)
    short = re.sub(r"【[^】]*】|\[[^\]]*\]", "", title).strip()
    short = re.split(r"\s+", short)[0][:18] or title[:16]

    if re.search(r"鞋|袜|运动服|T恤|裤|外套|卫衣", title):
        variants = [
            f"这款{short}收到试穿体验很不错，尺码标准按平时码数选刚好合脚，上脚轻便舒适不磨脚。面料做工扎实走线工整，透气排汗效果好，日常通勤和运动都能穿。版型百搭耐看，细节处理到位没有多余线头和异味，京东配送速度快包装完好，整体性价比很高。",
            f"入手的{short}质感超出预期，裁剪利落穿起来很合身，日常活动伸展自如没有束缚感。用料厚实亲肤，洗后不易变形起球，配色耐看实用。发货及时物流给力，打开检查做工精细无瑕疵，穿了一阵子脚感/体感都很舒服，值得回购推荐。",
        ]
    elif re.search(r"宝宝|婴儿|婴幼儿|辅食|米粉|奶粉|母乳|背奶|儿童|维生素|滴剂|尿不湿", title):
        variants = [
            f"给宝宝选的这款{short}品质很让人放心，包装密封严实没有破损，生产日期也很新鲜。材质安全无异味，细节设计贴心考虑到了宝宝日常使用习惯，实际用下来宝宝接受度很高，家里大人操作起来也很顺手省心。京东正品渠道有保障，物流送货快，用完还会继续回购。",
            f"回购的{short}一如既往地靠谱，成分/材质让人安心，拆封后干净卫生没有杂味。给宝宝使用温和贴心，做工细腻没有毛刺和瑕疵，日常带娃能省不少事。快递包装结实次日就送到了，价格结合活动很划算，家里常备母婴好物非常满意。",
        ]
    elif re.search(r"锅|碗|杯|壶|厨|洗碗|凝珠|清洁|收纳|毛巾|湿巾|纸", title):
        variants = [
            f"这款{short}买回家用了一段时间体验非常不错，材质厚实有分量，做工精细边角打磨光滑，拆开没有刺鼻异味。日常居家使用顺手实用，清洁打理起来也不费劲，确实能明显提升生活便利度。包装防护到位没有磕碰破损，京东送货速度快，性价比挺高。",
            f"家里日用刚需入手的{short}，实物和详情页介绍一致，用料正规耐用度不错。设计简洁实用，放在家里不占空间而且很耐看，实际使用效果让人满意，省时又省力。快递准时送达外箱完好，日常囤货很合适，下次有需要还会再买。",
        ]
    elif re.search(r"手环|手表|手机|耳机|充电|电源|数据线|开关|插座|电脑|键盘|鼠标|路由器", title):
        variants = [
            f"这款{short}到手试用非常满意，外观做工精致有质感，边角贴合严密没有廉价感。核心功能响应灵敏稳定，连接和设置过程都很顺畅，日常使用体验流畅省心。续航和散热表现也在预期之内，包装盒完整密封良好，京东发货快售后有保障，物有所值。",
            f"对比了好几款最后选的{short}，拿到手果然没失望，细节做工扎实，性能稳定可靠，日常高频使用也没出现卡顿或断连。操作界面直观好上手，整体颜值耐看百搭，快递次日达保护得很好，结合优惠入手性价比相当不错。",
        ]
    elif re.search(r"牛肉|零食|食品|茶|咖啡|饮|饼干|坚果|面|米|油|酱", title):
        variants = [
            f"这款{short}味道很不错，拆开包装香气自然，配料表看着也比较放心。口感扎实新鲜不腻口，咸淡火候掌握得恰到好处，平时当佐餐小菜或者追剧解馋都很合适。独立/密封包装干净卫生，生产日期很新鲜，快递送货快没压坏，吃完会再回购。",
        ]
    elif re.search(r"焕新|延保|碎屏|服务|保险|会员", title):
        variants = [
            f"下单商品时顺手搭配了{short}服务，生效速度很快，在订单详情里能清晰查看保障内容和有效期。日常使用难免担心意外磕碰或损坏，多一份官方售后保障心里踏实很多，整体费用实惠透明，虽然希望用不上，但有备无患非常让人安心。",
        ]
    else:
        variants = [
            f"这款{short}收到检查完好无损，实物颜值和质感都挺在线，做工细节处理得比较规整，用料扎实没有廉价感。实际使用下来功能稳定顺手，完全符合日常预期，包装严实保护到位。京东物流一如既往地快，整体购物体验很省心，五星好评推荐。",
            f"入手的{short}使用体验很满意，设计合理实用性高，拆箱没有异味和瑕疵，拿到手就能直接用上。细节看得出品牌用心，耐用度和手感都不错，日常使用频率很高。快递配送准时服务态度好，价格实惠性价比突出，有需要还会再来。",
        ]
    return variants[idx % len(variants)]


def dump_ui_xml(adb_serial: str) -> str:
    return subprocess.check_output(
        [
            "adb",
            "-s",
            adb_serial,
            "shell",
            "su -c 'uiautomator dump /data/local/tmp/ui_rev_auto.xml >/dev/null 2>&1 && cat /data/local/tmp/ui_rev_auto.xml'",
        ],
        text=True,
        timeout=15,
    )


def parse_bounds(b: str):
    m = re.match(r"\[(\d+),(\d+)\]\[(\d+),(\d+)\]", b or "")
    if not m:
        return None
    return tuple(int(x) for x in m.groups())


def parse_jd_nodes(xml: str) -> list:
    nodes = []
    for m in re.finditer(r"<node [^>]+>", xml):
        s = m.group(0)
        pkg = (re.search(r'package="([^"]*)"', s) or [None, ""])[1]
        if pkg and pkg != JD_PKG:
            continue
        txt = (re.search(r'text="([^"]*)"', s) or [None, ""])[1]
        desc = (re.search(r'content-desc="([^"]*)"', s) or [None, ""])[1]
        cls = (re.search(r'class="([^"]*)"', s) or [None, ""])[1]
        b = parse_bounds((re.search(r'bounds="([^"]*)"', s) or [None, ""])[1])
        if b:
            nodes.append({"text": txt, "desc": desc, "cls": cls, "bounds": b})
    return nodes


def get_floating_ssh_touch_rect(adb_serial: str):
    """Return (x1, y1, x2, y2) of the user's floating SSH window if present, else None."""
    try:
        out = subprocess.check_output(
            [
                "adb",
                "-s",
                adb_serial,
                "shell",
                "su -c 'dumpsys input | grep -E \"com\\.server\\.auditor.*touchableRegion=\" | head -n 1'",
            ],
            text=True,
            timeout=8,
        )
        m = re.search(r"touchableRegion=\[(\d+),(\d+)\]\[(\d+),(\d+)\]", out)
        if m:
            return tuple(int(x) for x in m.groups())
    except Exception:
        pass
    return None


def point_in_rect(x: int, y: int, rect) -> bool:
    if not rect:
        return False
    return rect[0] <= x <= rect[2] and rect[1] <= y <= rect[3]


def submit_single_review(mc: MCPClient, adb_serial: str, review_text: str, float_rect) -> dict:
    """Fill and submit the currently open CommentFloorActivity."""
    time.sleep(2.2)
    fg = mc.call_json("phone_foreground_app")
    if "CommentFloorActivity" not in (fg.get("activity") or ""):
        return {"ok": False, "reason": f"unexpected_activity:{fg.get('activity')}"}

    xml = dump_ui_xml(adb_serial)
    check_captcha(xml)
    nodes = parse_jd_nodes(xml)

    # 1. Optional size chip ('合适') if shown
    fit_node = next((n for n in nodes if n["text"] == "合适"), None)
    if fit_node:
        x1, y1, x2, y2 = fit_node["bounds"]
        cx, cy = (x1 + x2) // 2, (y1 + y2) // 2
        if not point_in_rect(cx, cy, float_rect):
            mc.call("phone_tap", {"x": cx, "y": cy, "coord_space": "screen"})
            time.sleep(0.35)

    # 2. Find EditText and paste review via clipboard (avoiding floating window on right)
    edit_node = next((n for n in nodes if "EditText" in n["cls"]), None)
    if not edit_node:
        return {"ok": False, "reason": "no_edittext_found"}
    ex1, ey1, ex2, ey2 = edit_node["bounds"]
    tap_x = min((ex1 + ex2) // 2, 460)
    tap_y = (ey1 + ey2) // 2
    mc.call("phone_tap", {"x": tap_x, "y": tap_y, "coord_space": "screen"})
    time.sleep(0.7)
    mc.call("phone_input_text", {"text": review_text, "method": "clipboard", "submit": False})
    time.sleep(0.7)
    # Close soft keyboard
    mc.call("phone_keyevent", {"keycode": "BACK"})
    time.sleep(0.7)

    # 3. Scroll gently to reveal positive tag chips & re-dump UI
    mc.call("phone_swipe", {"x1": 360, "y1": 1220, "x2": 360, "y2": 680, "duration_ms": 260, "coord_space": "image"})
    time.sleep(0.6)
    xml2 = dump_ui_xml(adb_serial)
    check_captcha(xml2)
    nodes2 = parse_jd_nodes(xml2)

    # Check written character count
    char_node = next((n for n in nodes2 if re.match(r"已写\d+字", n["text"])), None)
    char_info = char_node["text"] if char_node else "unknown"

    # Tap up to 3 positive tag chips between '商品评价' and '快递包装'/'匿名评价'
    rate_node = next((n for n in nodes2 if n["text"] == "商品评价"), None)
    pack_node = next((n for n in nodes2 if n["text"] in ("快递包装", "送货速度", "匿名评价")), None)
    tags_tapped = []
    if rate_node:
        ry_min = rate_node["bounds"][3] + 10
        ry_max = (pack_node["bounds"][1] - 10) if pack_node else (ry_min + 260)
        for n in nodes2:
            t = n["text"]
            x1, y1, x2, y2 = n["bounds"]
            if not t or len(t) < 2 or len(t) > 8:
                continue
            if t in (
                "非常好", "很好", "一般", "差", "非常差", "视频/图", "发布", "写评价", "匿名评价",
                "商品符合度", "店家服务态度", "快递配送速度", "快递包装", "送货速度", "配送员服务",
            ):
                continue
            if y1 >= ry_min and y2 <= ry_max:
                cx, cy = (x1 + x2) // 2, (y1 + y2) // 2
                if not point_in_rect(cx, cy, float_rect):
                    mc.call("phone_tap", {"x": cx, "y": cy, "coord_space": "screen"})
                    tags_tapped.append(t)
                    time.sleep(0.28)
                    if len(tags_tapped) >= 3:
                        break

    # 4. Tap 发布 (safely avoiding floating SSH window if it overlaps top-right)
    pub_node = next((n for n in nodes2 if n["text"] in ("发布", "提交")), None)
    if not pub_node:
        return {"ok": False, "reason": "publish_btn_not_found", "chars": char_info}
    px1, py1, px2, py2 = pub_node["bounds"]
    pcx, pcy = (px1 + px2) // 2, (py1 + py2) // 2
    if point_in_rect(pcx, pcy, float_rect):
        # Use exposed band strictly above floating window (y < float_rect[1]) and inside publish button [py1..py2]
        if py1 < float_rect[1] - 2:
            pcy = max(py1 + 2, float_rect[1] - 5)
        elif px2 > float_rect[2] + 4:
            pcx = min(px2 - 2, float_rect[2] + 6)
        else:
            return {"ok": False, "reason": "publish_btn_fully_occluded_by_floating_window"}

    mc.call("phone_tap", {"x": pcx, "y": pcy, "coord_space": "screen"})
    time.sleep(2.4)

    # Handle optional confirmation modal ('确认提交') or photo nag modal ('仍要发布'/'直接发布')
    fg_after = mc.call_json("phone_foreground_app")
    if "CommentFloorActivity" in (fg_after.get("activity") or ""):
        xml3 = dump_ui_xml(adb_serial)
        check_captcha(xml3)
        nodes3 = parse_jd_nodes(xml3)
        for label in ("不再提醒",):
            nd = next((n for n in nodes3 if n["text"] == label), None)
            if nd:
                x1, y1, x2, y2 = nd["bounds"]
                mc.call("phone_tap", {"x": (x1 + x2) // 2, "y": (y1 + y2) // 2, "coord_space": "screen"})
                time.sleep(0.35)
        confirm = next((n for n in nodes3 if n["text"] in ("确认提交", "仍要发布", "直接发布", "继续发布", "确认发布")), None)
        if confirm:
            x1, y1, x2, y2 = confirm["bounds"]
            mc.call("phone_tap", {"x": (x1 + x2) // 2, "y": (y1 + y2) // 2, "coord_space": "screen"})
            time.sleep(2.6)
            fg_after = mc.call_json("phone_foreground_app")

    ok = "CommentFloorActivity" not in (fg_after.get("activity") or "")
    return {
        "ok": ok,
        "chars": char_info,
        "tags": tags_tapped,
        "after_activity": fg_after.get("activity"),
    }


def cmd_review_list(mc: MCPClient, _args):
    require_guard_active(mc)
    tid = return_to_review_center(mc)
    state = read_review_center_state(mc, tid)
    print(json.dumps(state, ensure_ascii=False, indent=2))


def cmd_review_all(mc: MCPClient, args):
    require_guard_active(mc)
    bespoke_map = {}
    if args.reviews_json and Path(args.reviews_json).exists():
        bespoke_map = json.loads(Path(args.reviews_json).read_text(encoding="utf-8"))

    float_rect = get_floating_ssh_touch_rect(args.adb)
    tid = return_to_review_center(mc)
    tid = reload_review_center(mc, tid)
    before = read_review_center_state(mc, tid)
    before_summary = {"reviewed": before.get("reviewed"), "beans": before.get("beans")}

    submitted = []
    skipped = []
    attempted = set()

    for idx in range(args.max_count):
        tid = return_to_review_center(mc)
        if idx > 0:
            tid = reload_review_center(mc, tid)
        state = read_review_center_state(mc, tid)
        check_captcha(state.get("snippet", ""))
        cards = [c for c in (state.get("cards") or []) if c["title"] not in attempted]
        if not cards:
            # Scroll down once to load more items if any remain below fold
            mc.call("phone_swipe", {"x1": 360, "y1": 1300, "x2": 360, "y2": 450, "duration_ms": 300, "coord_space": "image"})
            time.sleep(1.2)
            state = read_review_center_state(mc, tid)
            cards = [c for c in (state.get("cards") or []) if c["title"] not in attempted]
            if not cards:
                break

        card = cards[0]
        title = card["title"]
        attempted.add(title)
        review_text = generate_quality_review(title, bespoke_map, idx)

        if args.dry_run:
            submitted.append({"title": title, "chars": len(review_text), "preview": review_text[:40]})
            continue

        if not click_review_card(mc, tid, title):
            skipped.append({"title": title, "reason": "click_failed"})
            continue

        res = submit_single_review(mc, args.adb, review_text, float_rect)
        entry = {"title": title[:48], "len": len(review_text), **res}
        if res.get("ok"):
            submitted.append(entry)
            print(f"[OK #{len(submitted)}] {title[:40]} ({res.get('chars')}, tags={res.get('tags')})", flush=True)
        else:
            skipped.append(entry)
            print(f"[SKIP] {title[:40]} -> {res.get('reason')}", flush=True)
            # Back out of CommentFloorActivity cleanly (dismissing any '放弃评价/离开' confirm dialog)
            mc.call("phone_keyevent", {"keycode": "BACK"})
            time.sleep(1.0)
            fg_chk = mc.call_json("phone_foreground_app")
            if "CommentFloorActivity" in (fg_chk.get("activity") or ""):
                try:
                    x_leave = dump_ui_xml(args.adb)
                    for nd in parse_jd_nodes(x_leave):
                        if nd["text"] in ("离开", "放弃", "确定离开", "暂不评价", "残忍离开"):
                            x1, y1, x2, y2 = nd["bounds"]
                            mc.call("phone_tap", {"x": (x1 + x2) // 2, "y": (y1 + y2) // 2, "coord_space": "screen"})
                            time.sleep(1.0)
                            break
                except Exception:
                    pass

        time.sleep(max(1.0, args.delay))

    tid = return_to_review_center(mc)
    tid = reload_review_center(mc, tid)
    after = read_review_center_state(mc, tid)
    after_summary = {"reviewed": after.get("reviewed"), "beans": after.get("beans")}
    report = {
        "before": before_summary,
        "after": after_summary,
        "submitted_count": len(submitted),
        "skipped_count": len(skipped),
        "submitted": submitted,
        "skipped": skipped,
        "remaining_visible": len(after.get("cards") or []),
    }
    print(json.dumps(report, ensure_ascii=False, indent=2))


def main():
    p = argparse.ArgumentParser(description="Xiaomi 13 JD daily automation via WebView CDP")
    p.add_argument("--host", default=DEFAULT_HOST)
    p.add_argument("--adb", default=DEFAULT_ADB)
    sub = p.add_subparsers(dest="cmd", required=True)

    sub.add_parser("status", help="Check daemon, WebView CDP sockets, and 东东农场 summary")

    p_all = sub.add_parser("farm-all", help="Run full 东东农场 daily water-accumulation flow (signin + safe browse + watering tasks)")
    p_all.add_argument("--water-mode", choices=["tasks", "all", "fixed"], default="tasks", help="tasks (default): only complete watering milestone tasks & save surplus water; all: pour all water; fixed: click --max-water-rounds times")
    p_all.add_argument("--max-water-rounds", type=int, default=30, help="Safety cap on watering clicks (default 30)")
    p_all.add_argument("--keep-water", type=int, default=0, help="Minimum water (g) to keep when water-mode=all")
    p_all.add_argument("--allow-mature", action="store_true", help="Allow watering through final harvest (default pauses <=5%% before harvest for user)")
    p_all.add_argument("--max-browse", type=int, default=24)
    p_all.add_argument("--browse-sec", type=float, default=8.5)
    p_all.add_argument("--skip-browse", action="store_true")

    p_w = sub.add_parser("farm-water", help="Water 东东农场 (default mode=tasks: finish watering milestone tasks only)")
    p_w.add_argument("--mode", choices=["tasks", "all", "fixed"], default="tasks")
    p_w.add_argument("--max-rounds", type=int, default=30)
    p_w.add_argument("--keep-water", type=int, default=0)
    p_w.add_argument("--allow-mature", action="store_true")

    sub.add_parser("bean-signin", help="Open 领京豆 H5 inside JD WebView and click daily sign-in")

    sub.add_parser("review-list", help="List pending JD orders in 评价中心 WebView")
    p_rev = sub.add_parser("review-all", help="Auto-submit high-quality 5-star text reviews (方案B) for pending JD orders")
    p_rev.add_argument("--max-count", type=int, default=30, help="Max pending orders to review (default 30)")
    p_rev.add_argument("--delay", type=float, default=2.5, help="Pause in seconds between reviews (default 2.5)")
    p_rev.add_argument("--reviews-json", default="", help="Optional JSON file mapping product-title substrings to bespoke review text")
    p_rev.add_argument("--dry-run", action="store_true", help="Preview generated reviews without submitting")

    args = p.parse_args()
    mc = MCPClient(args.host, resolve_token(args.adb))
    {
        "status": cmd_status,
        "farm-all": cmd_farm_all,
        "farm-water": cmd_farm_water,
        "bean-signin": cmd_bean_signin,
        "review-list": cmd_review_list,
        "review-all": cmd_review_all,
    }[args.cmd](mc, args)


if __name__ == "__main__":
    main()
