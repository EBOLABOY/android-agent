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
import urllib.request
from pathlib import Path

DEFAULT_HOST = os.environ.get("AGENT_HOST", "http://192.168.100.100:8765")
DEFAULT_ADB = os.environ.get("AGENT_ADB_SERIAL", "192.168.100.100:5555")
JD_PKG = "com.jingdong.app.mall"

CAPTCHA_RE = re.compile(
    r"安全验证|滑块验证|拖动滑块|人机验证|verify you are human|captcha|请完成验证",
    re.IGNORECASE,
)

# Tasks explicitly forbidden to touch (third-party installs, widgets, follow spam, paid orders)
FORBIDDEN_TASK_RE = re.compile(
    r"快手|极速版|小组件|关注|邀请|好友|下单|购买|支付|开通|PLUS|金融|借款|白条|信用卡|助力",
    re.IGNORECASE,
)

# Safe browse tasks allowed for automatic 12s view + claim
SAFE_BROWSE_RE = re.compile(
    r"浏览\d+秒|读\d+s|精选活动推荐|限时领|逛一逛",
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


def ensure_farm_open(mc: MCPClient) -> dict:
    """Bring JD foreground and ensure 东东农场 WebView target is live."""
    mc.call("phone_launch_app", {"package": JD_PKG})
    time.sleep(2.5)
    t = find_farm_target(mc)
    if t:
        return t

    # Open 东东农场 via JD search bar using Chinese clipboard input
    mc.call("phone_tap", {"x": 300, "y": 200, "coord_space": "image"})
    time.sleep(1.2)
    mc.call("phone_input_text", {"text": "东东农场", "method": "clipboard", "submit": True})
    for _ in range(10):
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
        time.sleep(1.2)
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
    time.sleep(0.8)


def claim_all_drawer_rewards(mc: MCPClient, tid: str) -> int:
    claimed = mc.eval_js(
        JD_PKG,
        tid,
        """
        (() => {
          let n = 0;
          for (const el of Array.from(document.querySelectorAll("div, span, button"))) {
            const t = (el.innerText || "").trim();
            const r = el.getBoundingClientRect();
            if ((t === "去领奖" || t === "领取") && r.width > 20 && r.height > 15 && r.top > 100) {
              el.click();
              n++;
            }
          }
          return n;
        })()
        """,
    )
    if claimed:
        time.sleep(1.0)
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


def do_safe_browse_tasks(mc: MCPClient, tid: str, max_runs: int = 12, browse_sec: float = 12.0) -> dict:
    completed = []
    if not open_task_drawer(mc, tid):
        return {"completed": completed, "claims": 0}
    claims = claim_all_drawer_rewards(mc, tid)

    for _ in range(max_runs):
        # Scan visible task cards in DOM and find first SAFE browse task with "去完成" / "逛一逛"
        cand = mc.eval_js(
            JD_PKG,
            tid,
            """
            (() => {
              const rows = [];
              for (const btn of Array.from(document.querySelectorAll("div, span, button"))) {
                const bt = (btn.innerText || "").trim();
                const br = btn.getBoundingClientRect();
                if (!/^(去完成|逛一逛)$/.test(bt) || br.width < 30 || br.height < 18 || br.top < 120) continue;
                // Climb to row container to read title + description
                let row = btn.parentElement;
                for (let k = 0; k < 4 && row; k++) {
                  if ((row.innerText || "").length > 15 && row.getBoundingClientRect().width > 240) break;
                  row = row.parentElement;
                }
                const rtxt = (row ? row.innerText : bt).replace(/\\s+/g, " ").trim();
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
            if SAFE_BROWSE_RE.search(rtxt):
                chosen = r
                break

        if not chosen:
            break

        # Click the chosen row's button via DOM
        clicked = mc.eval_js(
            JD_PKG,
            tid,
            f"""
            (() => {{
              const want = {json.dumps(chosen["rowText"][:24])};
              for (const btn of Array.from(document.querySelectorAll("div, span, button"))) {{
                const bt = (btn.innerText || "").trim();
                const br = btn.getBoundingClientRect();
                if (!/^(去完成|逛一逛)$/.test(bt) || br.width < 30 || br.top < 120) continue;
                let row = btn.parentElement;
                for (let k = 0; k < 4 && row; k++) {{
                  if ((row.innerText || "").length > 15 && row.getBoundingClientRect().width > 240) break;
                  row = row.parentElement;
                }}
                if (row && (row.innerText || "").replace(/\\s+/g, " ").includes(want)) {{
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

        # Wait for JD 6s countdown (3.5s load + gentle scroll + remainder)
        time.sleep(3.5)
        mc.call("phone_swipe", {"x1": 360, "y1": 1100, "x2": 360, "y2": 680, "duration_ms": 280, "coord_space": "image"})
        time.sleep(max(1.0, browse_sec - 3.5))
        mc.call("phone_keyevent", {"keycode": "BACK"})
        time.sleep(1.8)

        open_task_drawer(mc, tid)
        claims += claim_all_drawer_rewards(mc, tid)
        completed.append(chosen["rowText"][:60])

    close_task_drawer(mc, tid)
    return {"completed": completed, "claims": claims}


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
    t = ensure_farm_open(mc)
    tid = t["id"]
    report = {"before": read_farm_summary(mc, tid)}
    if report["before"].get("needPrize"):
        report["stopped_reason"] = "prize_selection_required"
        print(json.dumps(report, ensure_ascii=False, indent=2))
        return

    dismiss_popups(mc, tid)

    # 1. Collect tree floating 10g water bubble if present
    mc.eval_js(
        JD_PKG,
        tid,
        '(() => { const b = document.querySelector(".FixedTaskIcon_fixed_task_img__388lV"); if (b) b.click(); })()',
    )
    time.sleep(0.8)

    # 2. Daily sign-in
    report["signin"] = do_farm_signin(mc, tid)
    dismiss_popups(mc, tid)

    # 3. Safe 6s browse tasks (accumulate continuous water drops)
    if not args.skip_browse:
        report["browse_tasks"] = do_safe_browse_tasks(mc, tid, max_runs=args.max_browse, browse_sec=args.browse_sec)

    # 4. Complete watering milestone tasks ONLY (preserving surplus water & leaving final harvest watering for user)
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


def cmd_bean_signin(mc: MCPClient, _args):
    require_guard_active(mc)
    t = ensure_farm_open(mc)
    tid = t["id"]
    orig_url = t["url"]
    mc.cdp(JD_PKG, tid, "Page.navigate", {"url": "https://bean.m.jd.com/bean/signIndex.action"})
    time.sleep(3.5)
    res = mc.eval_js(
        JD_PKG,
        tid,
        """
        (() => {
          const txt = (document.body?.innerText || "").slice(0, 700);
          let clicked = null;
          const btn = Array.from(document.querySelectorAll("div, span, button, a")).find(e => {
            const s = (e.innerText || "").trim();
            const r = e.getBoundingClientRect();
            return /^(签到领京豆|立即签到|点击签到|签到)$/.test(s) && r.width > 20 && r.height > 15;
          });
          if (btn) { btn.click(); clicked = btn.innerText.trim(); }
          return {title: document.title, url: location.href, clicked, snippet: txt};
        })()
        """,
    )
    time.sleep(1.5)
    mc.cdp(JD_PKG, tid, "Page.navigate", {"url": orig_url})
    print(json.dumps(res, ensure_ascii=False, indent=2))


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
    p_all.add_argument("--max-browse", type=int, default=12)
    p_all.add_argument("--browse-sec", type=float, default=12.0)
    p_all.add_argument("--skip-browse", action="store_true")

    p_w = sub.add_parser("farm-water", help="Water 东东农场 (default mode=tasks: finish watering milestone tasks only)")
    p_w.add_argument("--mode", choices=["tasks", "all", "fixed"], default="tasks")
    p_w.add_argument("--max-rounds", type=int, default=30)
    p_w.add_argument("--keep-water", type=int, default=0)
    p_w.add_argument("--allow-mature", action="store_true")

    sub.add_parser("bean-signin", help="Open 领京豆 H5 inside JD WebView and click daily sign-in")

    args = p.parse_args()
    mc = MCPClient(args.host, resolve_token(args.adb))
    {
        "status": cmd_status,
        "farm-all": cmd_farm_all,
        "farm-water": cmd_farm_water,
        "bean-signin": cmd_bean_signin,
    }[args.cmd](mc, args)


if __name__ == "__main__":
    main()
