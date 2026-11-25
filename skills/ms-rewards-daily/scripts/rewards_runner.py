#!/usr/bin/env python3
"""
Microsoft Rewards daily automation runner for Xiaomi 13 via android-agent MCP Server.
Pure Python 3 standard library (zero external dependencies).
"""

import argparse
import base64
import datetime
import hashlib
import json
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

DEFAULT_HOST = os.environ.get("AGENT_HOST", "http://192.168.100.100:8765")
DEFAULT_ADB = os.environ.get("AGENT_ADB_SERIAL", "192.168.100.100:5555")
DEFAULT_OUT = "/tmp/ms-rewards-shots"

# Diverse pool of natural daily search queries (rotated deterministically by date)
SEARCH_QUERY_POOL = [
    "xiaomi 13 camera sensor size",
    "kyoto autumn foliage peak dates",
    "best pour over coffee water ratio",
    "how solid state batteries work",
    "tokyo ramen styles explained",
    "mechanical keyboard tactile vs linear",
    "swiss alps scenic train routes",
    "solar eclipse visibility calendar",
    "espresso extraction time and temperature",
    "japanese matcha grades guide",
    "usb4 vs thunderbolt 4 bandwidth",
    "marathon pacing strategy calculator",
    "history of porcelain tea ware",
    "home studio acoustic treatment basics",
    "oled vs mini led contrast ratio",
    "best indoor low light houseplants",
    "mediterranean diet weekly meal ideas",
    "how noise cancelling headphones work",
    "national parks in utah road trip",
    "sourdough bread hydration percentage",
    "python asyncio gather vs taskgroup",
    "linux kernel scheduler overview",
    "electric vehicle regenerative braking",
    "classical guitar nylon string tension",
    "nordic minimalist desk setup tips",
    "best hiking boots waterproofing wax",
    "how e-ink displays save power",
    "green tea brewing temperature chart",
    "hdr10 plus vs dolby vision differences",
    "wifi 7 multi link operation explained",
    "mechanical watch automatic movement",
    "best cast iron skillet seasoning oil",
    "tokyo tsukiji outer market food guide",
    "how geothermal heat pumps work",
    "camera aperture depth of field chart",
    "portable ssd sequential read speeds",
]

# Mapping from "Explore on Bing" card keywords (Chinese/English) to reliable Bing search queries
# NOTE: Place specific domain rules (cruise/flights/plans/insurance/loans) BEFORE generic "优惠/deals"!
EXPLORE_TOPIC_RULES = [
    (r"邮轮|启航|cruise", "best caribbean cruise deals and destinations"),
    (r"航班|机票|短途旅行|假期|flight|travel|trip|vacation", "flights from Los Angeles to Tokyo"),
    (r"保险|insurance", "best auto and home insurance plans"),
    (r"贷款|loan|mortgage", "compare personal and student loan rates"),
    (r"美容|护发|香氛|护肤|beauty|haircare|fragrance|skincare", "best skincare and fragrance gift sets"),
    (r"手机套餐|通话|短信|cell phone|phone plan|wireless plan", "best unlimited cell phone plans comparison"),
    (r"网络套餐|宽带|internet plan|broadband|wifi plan", "compare high speed home internet plans"),
    (r"餐厅|美食|做饭|晚饭|restaurant|dining|dinner|food", "best local restaurants near me"),
    (r"食谱|做菜|recipe|cook", "easy healthy weeknight dinner recipes"),
    (r"天气|weather|forecast", "weather forecast this week"),
    (r"酒店|住宿|hotel", "best boutique hotels in kyoto"),
    (r"汽车|新车|二手车|car|vehicle|suv", "best hybrid suv fuel economy comparison"),
    (r"电影|影院|movie|cinema|showtime", "top rated movies playing in theaters"),
    (r"工作|求职|简历|job|career|resume", "remote software engineer jobs"),
    (r"健身|锻炼|workout|fitness|exercise", "home strength training workout routine"),
    (r"歌词|歌曲|音乐|lyrics|song|music", "Shape of You Ed Sheeran lyrics"),
    (r"翻译|translate", "translate good morning to japanese"),
    (r"股市|股票|finance|stock|market", "sp 500 index performance today"),
    (r"健康|症状|health|wellness|vitamin", "daily stretching routine benefits"),
    (r"鞋|服装|fashion|shoes|sneakers", "best running shoes for daily training"),
    (r"购物|清单|优惠|shopping|deals|discount", "best shopping deals on wireless headphones"),
]

CAPTCHA_RE = re.compile(
    r"captcha|verify you are human|are you a robot|human verification|人机验证|请完成安全验证",
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


def get_battery_level(adb_serial: str):
    try:
        out = subprocess.check_output(
            ["adb", "-s", adb_serial, "shell", "dumpsys battery | grep level"],
            text=True,
            timeout=5,
        )
        m = re.search(r"level:\s*(\d+)", out)
        if m:
            return int(m.group(1))
    except Exception:
        pass
    return None


class MCPClient:
    def __init__(self, base_url: str, token: str):
        self.base_url = base_url.rstrip("/")
        self.token = token
        self.sid = None
        self.rpc_id = 0
        self._initialize()

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

    def _initialize(self):
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
        res = self.call(name, args)
        return json.loads(res["content"][0]["text"])

    def cdp(self, target_id: str, method: str, params: dict = None) -> dict:
        args = {"method": method}
        if target_id:
            args["target_id"] = target_id
        if params is not None:
            args["params"] = params
        return self.call_json("cdp_command", args)["result"]

    def eval_js(self, target_id: str, expr: str, await_promise: bool = False):
        params = {"expression": expr, "returnByValue": True}
        if await_promise:
            params["awaitPromise"] = True
        r = self.cdp(target_id, "Runtime.evaluate", params)
        if "exceptionDetails" in r:
            raise RuntimeError(f"JS exception: {json.dumps(r['exceptionDetails'], ensure_ascii=False)}")
        return r.get("result", {}).get("value")

    def screenshot(self, save_path: Path) -> dict:
        res = self.call("phone_screenshot")
        save_path.parent.mkdir(parents=True, exist_ok=True)
        save_path.write_bytes(base64.b64decode(res["content"][0]["data"]))
        return json.loads(res["content"][1]["text"])


def require_guard_active(mc: MCPClient) -> dict:
    st = mc.call_json("phone_status")
    guard = st.get("guard") or {}
    if guard.get("state") != "active":
        raise RuntimeError(f"Agent guard is not active (state={guard.get('state')}, detail={guard}); aborting safely.")
    return st


def ensure_edge_target(mc: MCPClient) -> str:
    mc.call("phone_launch_app", {"package": "com.microsoft.emmx"})
    for _ in range(8):
        cdp_st = mc.call_json("cdp_status")
        if cdp_st.get("available"):
            break
        time.sleep(1.0)
    else:
        raise RuntimeError("Edge CDP (@chrome_devtools_remote) did not become available after launching Edge.")

    targets = mc.call_json("cdp_list_targets").get("targets") or []
    pages = [t for t in targets if t.get("type") == "page" and (t.get("url") or "").startswith("http")]
    if not pages:
        pages = [t for t in targets if t.get("type") == "page"]
    if not pages:
        raise RuntimeError("No Edge page target available.")

    # Prefer the first active/topmost target (index 0 in Edge DevTools is the foreground active tab!)
    # or an existing rewards.bing.com/earn tab if it's responsive.
    for cand in pages[:3]:
        if "rewards.bing.com/earn" in (cand.get("url") or ""):
            try:
                mc.eval_js(cand["id"], "location.href")
                return cand["id"]
            except Exception:
                continue

    # Fall back to active foreground page (pages[0])
    tid = pages[0]["id"]
    mc.cdp(tid, "Page.bringToFront", {})
    return tid


def parse_earn_snapshot(mc: MCPClient, tid: str, open_modal: bool = True) -> dict:
    mc.cdp(tid, "Page.navigate", {"url": "https://rewards.bing.com/earn"})
    time.sleep(3.8)
    data = mc.eval_js(
        tid,
        f"""
        (async () => {{
          const rawText = document.body ? document.body.innerText : "";
          const captcha = {json.dumps(CAPTCHA_RE.pattern)};
          const hasCaptcha = new RegExp(captcha, "i").test(rawText.slice(0, 1200));
          const needLogin = /登录|Sign in/i.test(document.title) || location.hostname.includes("login.live.com");

          const lines = rawText.split("\\n").map(s => s.trim()).filter(Boolean);
          const availIdx = lines.findIndex(l => /^\\d{{1,3}}(,\\d{{3}})*$/.test(l));
          const availPts = availIdx >= 0 ? lines[availIdx] : null;
          const todayIdx = lines.findIndex(l => l === "今日积分");
          const todayPts = (todayIdx >= 0 && todayIdx + 1 < lines.length) ? lines[todayIdx + 1] : null;

          // Extract "在必应上浏览" activated cards
          const exploreCards = [];
          const dailyCards = [];
          const allLinks = Array.from(document.querySelectorAll("a"));
          for (const a of allLinks) {{
            const t = (a.innerText || "").replace(/\\s+/g, " ").trim();
            if (!t || t.length > 220) continue;
            const href = a.getAttribute("href") || "";
            if (t.includes("已激活") && !t.includes("已完成") && !t.includes("明天解锁")) {{
              exploreCards.push({{text: t, href}});
            }} else if (/\\+\\d+/.test(t) && !t.includes("已完成") && !t.includes("明天解锁") && !t.includes("已激活")) {{
              if (href.includes("bing.com") || href.includes("rewards.bing.com") || href.startsWith("/")) {{
                dailyCards.push({{text: t, href}});
              }}
            }}
          }}

          let breakdownText = "";
          if ({str(open_modal).lower()}) {{
            const btns = Array.from(document.querySelectorAll("button, a, [role=button]"));
            const detailBtn = btns.find(b => b.innerText && b.innerText.includes("积分明细"));
            if (detailBtn) {{
              detailBtn.click();
              await new Promise(r => setTimeout(r, 1200));
              const dialogs = Array.from(document.querySelectorAll("[role=dialog], [aria-modal=true]"));
              breakdownText = dialogs.map(d => d.innerText).join("\\n");
              const closeBtn = document.querySelector('[role=dialog] button[aria-label*="关闭"], [role=dialog] button[aria-label*="Close"], [role=dialog] button');
              if (closeBtn) closeBtn.click();
            }}
          }}

          const idxExplore = lines.findIndex(l => l === "在必应上浏览");
          const idxDaily = lines.findIndex(l => l === "日常任务");
          return {{
            url: location.href,
            title: document.title,
            hasCaptcha,
            needLogin,
            availablePoints: availPts,
            todayPoints: todayPts,
            exploreProgress: (idxExplore >= 0 && idxExplore + 1 < lines.length) ? lines[idxExplore + 1] : null,
            dailyProgress: (idxDaily >= 0 && idxDaily + 1 < lines.length) ? lines[idxDaily + 1] : null,
            exploreCards,
            dailyCards,
            breakdownText,
          }};
        }})()
        """,
        await_promise=True,
    )
    m = re.search(r"必应搜索\s+(\d+)/(\d+)", data.get("breakdownText") or "")
    data["bingSearchCurrent"] = int(m.group(1)) if m else None
    data["bingSearchCap"] = int(m.group(2)) if m else None
    return data


def map_explore_query(card_text: str) -> str:
    for pattern, query in EXPLORE_TOPIC_RULES:
        if re.search(pattern, card_text, re.IGNORECASE):
            return query
    # Fallback: strip boilerplate phrases and use remaining descriptive words
    cleaned = re.sub(r"在必应上?搜索|已激活|\+\d+|以找到|最适合您需求的", " ", card_text).strip()
    return cleaned[:48] if cleaned else "popular travel destinations"


def do_bing_search(mc: MCPClient, tid: str, query: str, wait_sec: float = 7.5) -> dict:
    mode = mc.eval_js(
        tid,
        f"""
        (() => {{
          const q = {json.dumps(query)};
          const input = document.querySelector("#sb_form_q, input[name=q]");
          const form = document.querySelector("#sb_form, form[action*='/search']");
          if (input && form) {{
            input.focus();
            input.value = q;
            input.dispatchEvent(new Event("input", {{bubbles: true}}));
            form.submit();
            return "form";
          }}
          location.href = "https://www.bing.com/search?q=" + encodeURIComponent(q) + "&form=QBRE&pc=EMMX01";
          return "nav";
        }})()
        """,
    )
    time.sleep(wait_sec)
    chk = mc.eval_js(
        tid,
        f"""
        (() => {{
          const txt = (document.body ? document.body.innerText : "").slice(0, 1000);
          return {{
            url: location.href,
            title: document.title,
            captcha: new RegExp({json.dumps(CAPTCHA_RE.pattern)}, "i").test(txt),
          }};
        }})()
        """,
    )
    chk["mode"] = mode
    chk["query"] = query
    if chk.get("captcha"):
        raise RuntimeError(f"CAPTCHA detected on Bing search ({query}); stopping immediately per safety policy.")
    return chk


def daily_rotated_queries(count: int) -> list:
    today_key = datetime.date.today().isoformat()
    seed = int(hashlib.sha256(today_key.encode()).hexdigest()[:8], 16)
    n = len(SEARCH_QUERY_POOL)
    offset = seed % n
    ordered = SEARCH_QUERY_POOL[offset:] + SEARCH_QUERY_POOL[:offset]
    return ordered[:count]


def cmd_status(mc: MCPClient, args):
    st = mc.call_json("phone_status")
    out = {
        "battery_percent": get_battery_level(args.adb),
        "phone_status": st,
    }
    if (st.get("guard") or {}).get("state") == "active" and (st.get("cdp") or {}).get("available"):
        try:
            tid = ensure_edge_target(mc)
            out["rewards_snapshot"] = parse_earn_snapshot(mc, tid, open_modal=True)
        except Exception as e:
            out["rewards_snapshot_error"] = str(e)
    print(json.dumps(out, ensure_ascii=False, indent=2))


def cdp_click_rect(mc: MCPClient, tid: str, rect: dict):
    x = rect["x"] + rect["w"] / 2.0
    y = rect["y"] + rect["h"] / 2.0
    mc.cdp(tid, "Input.dispatchMouseEvent", {"type": "mouseMoved", "x": x, "y": y})
    mc.cdp(tid, "Input.dispatchMouseEvent", {"type": "mousePressed", "x": x, "y": y, "button": "left", "clickCount": 1})
    mc.cdp(tid, "Input.dispatchMouseEvent", {"type": "mouseReleased", "x": x, "y": y, "button": "left", "clickCount": 1})


def complete_dashboard_daily_set(mc: MCPClient, tid: str) -> tuple:
    """Complete today's Daily Set cards on rewards.bing.com/dashboard.
    Returns (completed_list, interactive_manual_list).
    - Daily Poll (+10) & retryable 3-question Quiz (+30): auto-completed via real CDP clicks.
    - Accuracy-scored quizzes like 'This or That' (+50): NOT blind-clicked; returned in
      interactive_manual_list so the AI agent can answer each question accurately.
    """
    done = []
    manual = []
    mc.cdp(tid, "Page.navigate", {"url": "https://rewards.bing.com/dashboard"})
    time.sleep(3.5)
    cards = mc.eval_js(
        tid,
        """
        (() => {
          const res = [];
          for (const a of Array.from(document.querySelectorAll("a"))) {
            const href = a.getAttribute("href") || "";
            const txt = (a.innerText || "").replace(/\\s+/g, " ").trim();
            if (!txt || txt.includes("已完成") || !/\\+\\d+/.test(txt)) continue;
            if (href.includes("DailySet") || href.includes("RewardsDailyPoll") || href.includes("REWARDSQUIZ")) {
              res.push({text: txt.slice(0, 120), href});
            }
          }
          return res;
        })()
        """,
    ) or []

    for card in cards:
        txt = card["text"]
        href = card["href"]
        is_this_or_that = bool(re.search(r"this\s*or\s*that|二选一|\+50", txt, re.IGNORECASE)) or "ThisOrThat" in href
        if is_this_or_that:
            manual.append({"reason": "accuracy_scored_quiz_requires_ai_reasoning", **card})
            continue

        is_poll = "Poll" in href or "投票" in txt or "poll" in txt.lower()
        is_quiz = "Quiz" in href or "QUIZ" in href or "问答" in txt or "测验" in txt

        # Always click from dashboard first so Rewards registers card activation
        mc.cdp(tid, "Page.navigate", {"url": "https://rewards.bing.com/dashboard"})
        time.sleep(2.2)
        hint = txt.split(" ")[0][:12]
        mc.eval_js(
            tid,
            f"""
            (() => {{
              const el = Array.from(document.querySelectorAll("a")).find(a => a.innerText && a.innerText.includes({json.dumps(hint)}));
              if (el) {{ el.scrollIntoView({{block: "center"}}); el.click(); return true; }}
              return false;
            }})()
            """,
        )
        time.sleep(3.5)

        if is_poll:
            pick = mc.eval_js(
                tid,
                """
                (() => {
                  const opts = Array.from(document.querySelectorAll("acf-button-standard.btom_opt, .btom_opts > *, #btoption0"));
                  const el = opts[0];
                  if (!el) return null;
                  el.scrollIntoView({block: "center"});
                  const r = el.getBoundingClientRect();
                  return {text: (el.innerText || "").trim(), x: r.left, y: r.top, w: r.width, h: r.height};
                })()
                """,
            )
            if pick and pick.get("w", 0) > 0:
                cdp_click_rect(mc, tid, pick)
                time.sleep(2.0)
            done.append({"type": "poll", "card": txt[:60]})
        elif is_quiz:
            tried_by_q = {}
            for _ in range(15):
                info = mc.eval_js(
                    tid,
                    """
                    (() => {
                      const box = document.querySelector(".btom_opts")?.closest("li, div.b_ans") || document.body;
                      const t = (box.innerText || "").slice(0, 600);
                      if (/30\\/30|干得好|Great job|You've earned|你已获得|获得 30/i.test(t)) return {done: true};
                      // Check if a "Next / 下一题" button is visible and click it to advance immediately
                      const nextEl = Array.from(document.querySelectorAll("acf-button-standard, button, a")).find(e => {
                        const s = (e.innerText || "").trim();
                        return /^(下一题|下一步|下一个|Next|Next question)$/i.test(s) && e.getBoundingClientRect().width > 0;
                      });
                      if (nextEl) {
                        const r = nextEl.getBoundingClientRect();
                        return {done: false, nextBtn: {x: r.left, y: r.top, w: r.width, h: r.height}};
                      }
                      const opts = Array.from(document.querySelectorAll("acf-button-standard.btom_opt, .btom_opts > *"));
                      if (!opts.length) return {done: false, empty: true};
                      opts[0].scrollIntoView({block: "center"});
                      const items = opts.map(e => {
                        const r = e.getBoundingClientRect();
                        return {
                          text: (e.innerText || "").trim(),
                          correctAttr: e.getAttribute("iscorrectoption") || "",
                          x: r.left, y: r.top, w: r.width, h: r.height
                        };
                      });
                      const qKey = t.split("\\n").slice(0, 3).join("|");
                      return {done: false, qKey, items};
                    })()
                    """,
                )
                if info.get("done") or info.get("empty"):
                    break
                if info.get("nextBtn"):
                    cdp_click_rect(mc, tid, info["nextBtn"])
                    time.sleep(1.2)
                    continue
                items = info.get("items") or []
                tried = tried_by_q.setdefault(info.get("qKey", ""), set())
                pick = next((it for it in items if it.get("correctAttr", "").lower() == "true" and it["w"] > 0), None)
                if not pick:
                    pick = next((it for it in items if it["text"] not in tried and it["w"] > 0), items[0] if items else None)
                if not pick:
                    break
                tried.add(pick["text"])
                cdp_click_rect(mc, tid, pick)
                time.sleep(1.5)
            done.append({"type": "quiz", "card": txt[:60]})
        else:
            done.append({"type": "click", "card": txt[:60]})
    return done, manual


def cmd_edge_all(mc: MCPClient, args):
    st = require_guard_active(mc)
    battery = get_battery_level(args.adb)
    tid = ensure_edge_target(mc)
    report = {
        "battery_percent": battery,
        "target_id": tid,
        "dashboard_daily_set": [],
        "daily_clicked": [],
        "interactive_tasks": [],
        "explore_done": [],
        "searches_executed": [],
        "search_status": "skipped",
    }

    before = parse_earn_snapshot(mc, tid, open_modal=True)
    report["before"] = {
        "availablePoints": before["availablePoints"],
        "todayPoints": before["todayPoints"],
        "dailyProgress": before["dailyProgress"],
        "exploreProgress": before["exploreProgress"],
        "bingSearch": f"{before['bingSearchCurrent']}/{before['bingSearchCap']}",
    }
    if before["hasCaptcha"] or before["needLogin"]:
        report["stopped_reason"] = "captcha_or_login_required"
        print(json.dumps(report, ensure_ascii=False, indent=2))
        return

    # 0. Complete Dashboard Daily Set (3 cards: offer + mobile Shadow-DOM quiz + daily poll)
    ds_done, ds_manual = complete_dashboard_daily_set(mc, tid)
    report["dashboard_daily_set"] = ds_done
    report["interactive_tasks"].extend(ds_manual)
    mc.cdp(tid, "Page.navigate", {"url": "https://rewards.bing.com/earn"})
    time.sleep(3.0)

    # 1. Click non-interactive daily cards (+5/+10/+15)
    interactive_re = re.compile(r"问答|测验|quiz|poll|投票|选择", re.IGNORECASE)
    for card in before["dailyCards"]:
        txt = card["text"]
        if interactive_re.search(txt):
            report["interactive_tasks"].append(card)
            continue
        title_hint = txt.split(" ")[0][:16]
        # Ensure we are on /earn before clicking
        if "/earn" not in (mc.eval_js(tid, "location.href") or ""):
            mc.cdp(tid, "Page.navigate", {"url": "https://rewards.bing.com/earn"})
            time.sleep(3.0)
        clicked = mc.eval_js(
            tid,
            f"""
            (() => {{
              const hint = {json.dumps(title_hint)};
              const el = Array.from(document.querySelectorAll("a")).find(a => a.innerText && a.innerText.includes(hint));
              if (!el) return false;
              el.scrollIntoView({{block: "center"}});
              el.click();
              return true;
            }})()
            """,
        )
        if clicked:
            time.sleep(5.5)
            report["daily_clicked"].append(txt[:60])

    # 2. Complete activated "Explore on Bing" cards (with secondary retry if still uncompleted)
    if before["exploreCards"]:
        mc.cdp(
            tid,
            "Page.navigate",
            {"url": "https://www.bing.com/?form=ML2PCR&OCID=ML2PCR&PUBL=RewardsDO&CREA=ML2PCR&PC=ML2PCR"},
        )
        time.sleep(3.0)
        for card in before["exploreCards"]:
            q = map_explore_query(card["text"])
            res = do_bing_search(mc, tid, q, wait_sec=args.search_interval)
            report["explore_done"].append({"card": card["text"][:60], "query": q, "title": res["title"][:40]})

        retry_chk = parse_earn_snapshot(mc, tid, open_modal=False)
        for card in retry_chk.get("exploreCards") or []:
            txt_c = card["text"]
            alt_q = "周杰伦 稻香 歌词" if re.search(r"歌词|lyrics|song", txt_c, re.I) else (txt_c.split(" ")[0] + " official")
            res = do_bing_search(mc, tid, alt_q, wait_sec=args.search_interval)
            report["explore_done"].append({"card": txt_c[:60], "query": alt_q, "retry": True, "title": res["title"][:40]})

    # 3. Check Bing Search cap and finish remaining searches in safe batches (with cooldown detection)
    mid = parse_earn_snapshot(mc, tid, open_modal=True)
    cur = mid.get("bingSearchCurrent")
    cap = mid.get("bingSearchCap") or 100
    if cur is not None and cur < cap:
        pool = daily_rotated_queries(len(SEARCH_QUERY_POOL))
        pool_idx = 0
        report["search_status"] = "in_progress"
        while cur < cap and pool_idx < len(pool):
            remaining_pts = cap - cur
            batch_n = min(max(1, (remaining_pts + 4) // 5), 6, len(pool) - pool_idx)
            mc.cdp(tid, "Page.navigate", {"url": "https://www.bing.com/"})
            time.sleep(2.5)
            for _ in range(batch_n):
                q = pool[pool_idx]
                pool_idx += 1
                res = do_bing_search(mc, tid, q, wait_sec=args.search_interval)
                report["searches_executed"].append(q)

            check = parse_earn_snapshot(mc, tid, open_modal=True)
            new_cur = check.get("bingSearchCurrent")
            if new_cur is None:
                report["search_status"] = "unknown_counter"
                break
            if new_cur <= cur:
                report["search_status"] = f"cooldown_detected_at_{new_cur}/{cap}"
                mid = check
                break
            cur = new_cur
            mid = check
        if cur is not None and cur >= cap:
            report["search_status"] = "completed"
    elif cur is not None and cur >= cap:
        report["search_status"] = "already_completed"

    # Leave earn page cleanly open with latest state
    after = mid
    report["after"] = {
        "availablePoints": after["availablePoints"],
        "todayPoints": after["todayPoints"],
        "dailyProgress": after["dailyProgress"],
        "exploreProgress": after["exploreProgress"],
        "bingSearch": f"{after['bingSearchCurrent']}/{after['bingSearchCap']}",
    }
    print(json.dumps(report, ensure_ascii=False, indent=2))


def ensure_bing_app(mc: MCPClient):
    fg = mc.call_json("phone_foreground_app")
    if fg.get("package") != "com.microsoft.bing":
        mc.call("phone_launch_app", {"package": "com.microsoft.bing"})
        time.sleep(2.2)


def open_bing_rewards_and_capture(mc: MCPClient, out_dir: Path, prefix: str) -> dict:
    ensure_bing_app(mc)
    # Tap Home twice to jump to top of Bing Home
    mc.call("phone_tap", {"x": 86, "y": 1515, "coord_space": "image"})
    time.sleep(0.8)
    mc.call("phone_tap", {"x": 86, "y": 1515, "coord_space": "image"})
    time.sleep(1.2)
    # Tap bottom-left Rewards card
    mc.call("phone_tap", {"x": 150, "y": 1270, "coord_space": "image"})
    time.sleep(2.5)
    # Pull-to-refresh on the left side so Bing App never displays stale "1日前" cached points
    mc.call("phone_swipe", {"x1": 260, "y1": 460, "x2": 260, "y2": 1150, "duration_ms": 260, "coord_space": "image"})
    time.sleep(2.8)

    p1 = out_dir / f"{prefix}_1.jpg"
    p2 = out_dir / f"{prefix}_2.jpg"
    mc.screenshot(p1)
    mc.call("phone_swipe", {"x1": 260, "y1": 1200, "x2": 260, "y2": 500, "duration_ms": 300, "coord_space": "image"})
    time.sleep(1.0)
    mc.screenshot(p2)
    return {"top_screenshot": str(p1), "lower_screenshot": str(p2)}


def cmd_bing_check(mc: MCPClient, args):
    require_guard_active(mc)
    res = open_bing_rewards_and_capture(mc, Path(args.out_dir), "bing_check")
    res["battery_percent"] = get_battery_level(args.adb)
    print(json.dumps(res, ensure_ascii=False, indent=2))


def detect_checkin_day_x(img_path: Path) -> int:
    """Scan the 7 day-coin slots at y=916 in 720x1600 Rewards panel to find the active claimable Day coin."""
    slots = [88, 178, 270, 360, 450, 540, 630]
    try:
        from PIL import Image
        import numpy as np

        arr = np.asarray(Image.open(img_path).convert("RGB"), dtype=np.float32)
        for sx in slots:
            patch = arr[896:936, sx - 20 : sx + 20]
            # Claimable today's coin has a thin outer ring / light warm gold fill (higher brightness than solid orange checked)
            r, g, b = patch[:, :, 0], patch[:, :, 1], patch[:, :, 2]
            pale_gold = np.mean((r > 235) & (g > 205) & (g < 245) & (b > 130) & (b < 200))
            if pale_gold > 0.25:
                return sx
    except Exception:
        pass
    return 0


def cmd_bing_checkin(mc: MCPClient, args):
    require_guard_active(mc)
    out_dir = Path(args.out_dir)
    ensure_bing_app(mc)
    # Navigate to top of refreshed Rewards panel
    mc.call("phone_tap", {"x": 86, "y": 1515, "coord_space": "image"})
    time.sleep(0.8)
    mc.call("phone_tap", {"x": 86, "y": 1515, "coord_space": "image"})
    time.sleep(1.0)
    mc.call("phone_tap", {"x": 150, "y": 1270, "coord_space": "image"})
    time.sleep(2.5)
    mc.call("phone_swipe", {"x1": 260, "y1": 460, "x2": 260, "y2": 1150, "duration_ms": 260, "coord_space": "image"})
    time.sleep(2.6)

    pre = out_dir / "bing_checkin_pre.jpg"
    mc.screenshot(pre)
    day_x = 0
    if getattr(args, "day", 0) and 1 <= args.day <= 7:
        day_x = [88, 178, 270, 360, 450, 540, 630][args.day - 1]
    else:
        day_x = detect_checkin_day_x(pre)
        if not day_x:
            # Expand 签入 row first, then re-detect
            mc.call("phone_tap", {"x": 260, "y": 805, "coord_space": "image"})
            time.sleep(1.0)
            mc.screenshot(pre)
            day_x = detect_checkin_day_x(pre) or 260

    mc.call("phone_tap", {"x": day_x, "y": 916, "coord_space": "image"})
    time.sleep(2.2)
    p = out_dir / "bing_checkin_after.jpg"
    mc.screenshot(p)
    print(json.dumps({"screenshot": str(p), "tapped_x": day_x}, ensure_ascii=False, indent=2))


def scroll_article_to_bottom(mc: MCPClient, flings: int = 4):
    """Fast-fling to the very bottom of a Bing news article so Read-to-Earn (+3) triggers reliably."""
    for _ in range(flings):
        mc.call("phone_swipe", {"x1": 260, "y1": 1340, "x2": 260, "y2": 200, "duration_ms": 140, "coord_space": "image"})
        time.sleep(0.35)
    time.sleep(0.8)


def find_text_article_tap_ys(img_path: Path) -> list:
    """Detect visible pure-text news article tap Y-coords in a 720x1600 Bing feed screenshot,
    excluding video cards (white play triangle inside dark translucent circle) and partially scrolled-off top cards.
    """
    try:
        from PIL import Image
        import numpy as np
    except ImportError:
        return [810]

    arr = np.asarray(Image.open(img_path).convert("RGB"), dtype=np.float32)
    h, w, _ = arr.shape
    if h < 1450 or w < 700:
        return [810]
    gray = arr.mean(axis=2)

    # 1. Detect all video ▶ play-icon centers near x=360
    play_ys = []
    for y in range(240, min(1430, h - 56), 10):
        core = arr[y - 10 : y + 11, 348:368]
        white_ratio = float(np.mean((core.min(axis=2) > 220) & ((core.max(axis=2) - core.min(axis=2)) < 25)))
        if white_ratio < 0.72:
            continue
        yy, xx = np.ogrid[y - 55 : y + 56, 360 - 55 : 360 + 56]
        dist2 = (yy - y) ** 2 + (xx - 360) ** 2
        ring_mask = (dist2 >= 36**2) & (dist2 <= 52**2)
        ring = gray[y - 55 : y + 56, 360 - 55 : 360 + 56][ring_mask]
        ring_dark = float(np.mean(ring < 135))
        left_dark = float(np.mean(gray[y - 12 : y + 13, 322:338] < 135))
        if ring_dark > 0.65 and left_dark > 0.75:
            if not play_ys or y - play_ys[-1] > 60:
                play_ys.append(y)

    # 2. Detect contiguous thumbnail blocks across x=60..660, y=240..1420
    row_slice = arr[240:1420, 60:660]
    row_gray = row_slice.mean(axis=2)
    # Thumbnail rows have wide photo/artwork variation across columns (higher horizontal std or darker than #F5F6F8 feed bg)
    is_thumb_row = (row_gray.mean(axis=1) < 236) & (row_gray.std(axis=1) > 16)
    blocks = []
    in_blk = False
    start = 0
    gap = 0
    for idx, flag in enumerate(is_thumb_row):
        y = 240 + idx
        if flag:
            if not in_blk:
                in_blk = True
                start = y
            gap = 0
        elif in_blk:
            gap += 1
            if gap > 14:
                end = y - gap
                if 180 <= (end - start) <= 520:
                    blocks.append((start, end))
                in_blk = False
    if in_blk:
        end = 1420
        if 180 <= (end - start) <= 520:
            blocks.append((start, end))

    # 3. Keep only full-thumbnail non-video cards (block_y1 >= 255 ensures ▶ wasn't clipped above top edge)
    tap_ys = []
    for by1, by2 in blocks:
        if by1 < 255:
            continue
        if any((by1 - 25) <= py <= (by2 + 25) for py in play_ys):
            continue
        # Tap headline right below thumbnail if visible, else center of thumbnail (x=260 avoids floating SSH)
        tap_y = (by2 + 75) if (by2 + 115 <= 1400) else ((by1 + by2) // 2)
        if 320 <= tap_y <= 1360:
            tap_ys.append(int(tap_y))
    return tap_ys


def cmd_bing_read(mc: MCPClient, args):
    require_guard_active(mc)
    out_dir = Path(args.out_dir)
    ensure_bing_app(mc)
    # Jump cleanly to Bing Home top and swipe into news feed
    mc.call("phone_tap", {"x": 86, "y": 1515, "coord_space": "image"})
    time.sleep(0.8)
    mc.call("phone_tap", {"x": 86, "y": 1515, "coord_space": "image"})
    time.sleep(1.0)
    mc.call("phone_swipe", {"x1": 260, "y1": 1350, "x2": 260, "y2": 320, "duration_ms": 240, "coord_space": "image"})
    time.sleep(0.8)

    for _ in range(args.skip):
        mc.call("phone_swipe", {"x1": 260, "y1": 1350, "x2": 260, "y2": 280, "duration_ms": 190, "coord_space": "image"})
        time.sleep(0.4)

    read_ok = 0
    video_or_empty_scrolls = 0
    external_skips = 0
    max_scrolls = max(18, args.count * 3)
    feed_shot = out_dir / "bing_read_scan.jpg"

    for _ in range(max_scrolls):
        if read_ok >= args.count:
            break
        mc.screenshot(feed_shot)
        candidates = find_text_article_tap_ys(feed_shot)
        if not candidates:
            video_or_empty_scrolls += 1
        for tap_y in candidates:
            if read_ok >= args.count:
                break
            mc.call("phone_tap", {"x": 260, "y": tap_y, "coord_space": "image"})
            time.sleep(2.1)

            fg = mc.call_json("phone_foreground_app")
            pkg = fg.get("package") or ""
            if pkg not in ("com.microsoft.bing", "com.server.auditor.ssh.client", ""):
                external_skips += 1
                mc.call("phone_keyevent", {"keycode": "BACK"})
                time.sleep(0.7)
                ensure_bing_app(mc)
                continue

            scroll_article_to_bottom(mc, flings=args.flings)
            read_ok += 1
            mc.call("phone_keyevent", {"keycode": "BACK"})
            time.sleep(0.9)

        if read_ok < args.count:
            mc.call("phone_swipe", {"x1": 260, "y1": 1340, "x2": 260, "y2": 220, "duration_ms": 230, "coord_space": "image"})
            time.sleep(0.8)

    after = open_bing_rewards_and_capture(mc, out_dir, "bing_read_after")
    print(
        json.dumps(
            {
                "attempted": args.count,
                "read_ok": read_ok,
                "video_or_empty_scrolls": video_or_empty_scrolls,
                "external_skips": external_skips,
                "battery_percent": get_battery_level(args.adb),
                **after,
            },
            ensure_ascii=False,
            indent=2,
        )
    )


def cmd_bing_feed_shot(mc: MCPClient, args):
    require_guard_active(mc)
    out_dir = Path(args.out_dir)
    ensure_bing_app(mc)
    mc.call("phone_tap", {"x": 86, "y": 1515, "coord_space": "image"})
    time.sleep(1.0)
    for _ in range(args.swipes):
        mc.call("phone_swipe", {"x1": 260, "y1": 1350, "x2": 260, "y2": 300, "duration_ms": 220, "coord_space": "image"})
        time.sleep(0.5)
    p = out_dir / "bing_feed.jpg"
    mc.screenshot(p)
    print(json.dumps({"screenshot": str(p)}, ensure_ascii=False, indent=2))


def cmd_bing_tap_article(mc: MCPClient, args):
    require_guard_active(mc)
    ensure_bing_app(mc)
    mc.call("phone_tap", {"x": args.x, "y": args.y, "coord_space": "image"})
    time.sleep(1.8)
    scroll_article_to_bottom(mc, flings=args.flings)
    mc.call("phone_keyevent", {"keycode": "BACK"})
    time.sleep(0.8)
    print(json.dumps({"ok": True, "tapped": [args.x, args.y], "flings": args.flings}))


def cmd_edge_quiz_inspect(mc: MCPClient, args):
    require_guard_active(mc)
    tid = ensure_edge_target(mc)
    if args.url:
        mc.cdp(tid, "Page.navigate", {"url": args.url})
        time.sleep(3.5)
    info = mc.eval_js(
        tid,
        """
        (() => {
          const opts = Array.from(document.querySelectorAll("acf-button-standard.btom_opt, .btom_opts > *, [id^=rqAnswerOption], .btOptionCard"));
          return {
            url: location.href,
            title: document.title,
            snippet: (document.body ? document.body.innerText : "").slice(0, 800),
            options: opts.map(e => {
              const r = e.getBoundingClientRect();
              return {text: (e.innerText || "").trim(), x: r.left, y: r.top, w: r.width, h: r.height};
            })
          };
        })()
        """,
    )
    print(json.dumps(info, ensure_ascii=False, indent=2))


def cmd_edge_quiz_click(mc: MCPClient, args):
    require_guard_active(mc)
    tid = ensure_edge_target(mc)
    pick = mc.eval_js(
        tid,
        f"""
        (() => {{
          const target = {json.dumps(args.text)}.toLowerCase();
          const opts = Array.from(document.querySelectorAll("acf-button-standard.btom_opt, .btom_opts > *, [id^=rqAnswerOption], .btOptionCard, acf-button-standard, button"));
          const el = opts.find(e => (e.innerText || "").toLowerCase().includes(target) && e.getBoundingClientRect().width > 0);
          if (!el) return null;
          el.scrollIntoView({{block: "center"}});
          const r = el.getBoundingClientRect();
          return {{text: (el.innerText || "").trim(), x: r.left, y: r.top, w: r.width, h: r.height}};
        }})()
        """,
    )
    if not pick:
        raise RuntimeError(f"Option containing {args.text!r} not found")
    cdp_click_rect(mc, tid, pick)
    time.sleep(1.8)
    print(json.dumps({"clicked": pick["text"], "ok": True}, ensure_ascii=False))


def main():
    p = argparse.ArgumentParser(description="Xiaomi 13 Microsoft Rewards daily automation runner")
    p.add_argument("--host", default=DEFAULT_HOST)
    p.add_argument("--adb", default=DEFAULT_ADB)
    p.add_argument("--out-dir", default=DEFAULT_OUT)
    sub = p.add_subparsers(dest="cmd", required=True)

    sub.add_parser("status", help="Check daemon, guard, battery, and Rewards summary")

    p_edge = sub.add_parser("edge-all", help="Run all Edge CDP Rewards tasks (daily + explore + search)")
    p_edge.add_argument("--search-interval", type=float, default=7.5)

    sub.add_parser("bing-check", help="Open Bing App Rewards panel and save screenshots")
    p_ci = sub.add_parser("bing-checkin", help="Tap Bing App daily check-in and screenshot")
    p_ci.add_argument("--day", type=int, default=0, help="Optional Day 1-7 slot override (0=auto detect)")

    p_read = sub.add_parser("bing-read", help="Read text-only news articles in Bing App (filters out video cards)")
    p_read.add_argument("--count", type=int, default=11)
    p_read.add_argument("--skip", type=int, default=0)
    p_read.add_argument("--flings", type=int, default=4, help="Fast downward flings per article to reach bottom")

    p_feed = sub.add_parser("bing-feed-shot", help="Scroll Bing news feed and capture screenshot")
    p_feed.add_argument("--swipes", type=int, default=3)

    p_tap = sub.add_parser("bing-tap-article", help="Tap specific article (image coords), fling to bottom, return")
    p_tap.add_argument("--x", type=int, required=True)
    p_tap.add_argument("--y", type=int, required=True)
    p_tap.add_argument("--flings", type=int, default=3)

    p_qi = sub.add_parser("edge-quiz-inspect", help="Inspect current/target quiz question and options in Edge")
    p_qi.add_argument("--url", default="")

    p_qc = sub.add_parser("edge-quiz-click", help="Click a specific quiz option by text via real CDP mouse event")
    p_qc.add_argument("--text", required=True)

    args = p.parse_args()
    token = resolve_token(args.adb)
    mc = MCPClient(args.host, token)

    dispatch = {
        "status": cmd_status,
        "edge-all": cmd_edge_all,
        "bing-check": cmd_bing_check,
        "bing-checkin": cmd_bing_checkin,
        "bing-read": cmd_bing_read,
        "bing-feed-shot": cmd_bing_feed_shot,
        "bing-tap-article": cmd_bing_tap_article,
        "edge-quiz-inspect": cmd_edge_quiz_inspect,
        "edge-quiz-click": cmd_edge_quiz_click,
    }
    dispatch[args.cmd](mc, args)


if __name__ == "__main__":
    main()
