#!/usr/bin/env python3
"""
Unified ~4.5-minute one-shot daily automation runner for Xiaomi 13:
  1. Microsoft Rewards (Edge CDP + Bing App WebView CDP, 0 screenshots)
  2. JD Daily (东东农场 water/browse/milestone watering + 天天刮京豆)
Pure Python 3 standard library. Stops immediately on non-active guard or CAPTCHA.
"""

import argparse
import json
import os
import re
import subprocess
import sys
import time
from pathlib import Path

DEFAULT_HOST = os.environ.get("AGENT_HOST", "http://127.0.0.1:8765")
DEFAULT_ADB = os.environ.get("AGENT_ADB_SERIAL", "192.168.100.100:5555")

ROOT = Path(__file__).resolve().parent.parent
REWARDS_RUNNER = Path("/root/.agents/skills/ms-rewards-daily/scripts/rewards_runner.py")
JD_RUNNER = Path("/root/.agents/skills/jd-daily/scripts/jd_runner.py")
if not REWARDS_RUNNER.exists():
    REWARDS_RUNNER = ROOT / "skills" / "ms-rewards-daily" / "scripts" / "rewards_runner.py"
if not JD_RUNNER.exists():
    JD_RUNNER = ROOT / "skills" / "jd-daily" / "scripts" / "jd_runner.py"

STOP_TEXT_RE = re.compile(
    r"guard\s+is\s+not\s+active|guard\s+blocked|captcha|verify\s+you\s+are\s+human|human\s+verification|安全验证|滑块验证|拖动滑块|人机验证|请完成安全验证",
    re.IGNORECASE,
)


def run_step(label: str, cmd: list) -> dict:
    t0 = time.monotonic()
    print(f"\n=== [{label}] {' '.join(cmd)} ===", flush=True)
    proc = subprocess.run(cmd, text=True, capture_output=True)
    dt = round(time.monotonic() - t0, 1)
    if proc.stdout.strip():
        print(proc.stdout.strip(), flush=True)
    if proc.stderr.strip():
        print(proc.stderr.strip(), file=sys.stderr, flush=True)
    parsed = None
    for candidate in (proc.stdout.strip(),):
        idx = candidate.rfind("\n{")
        blob = candidate[idx + 1 :] if idx >= 0 else candidate
        if blob.startswith("{"):
            try:
                parsed = json.loads(blob)
            except Exception:
                pass
    stopped_reason = (parsed or {}).get("stopped_reason") or ""
    combined_out = f"{proc.stdout}\n{proc.stderr}\n{stopped_reason}"
    hard_stop = bool(STOP_TEXT_RE.search(combined_out)) or ("captcha" in stopped_reason.lower())
    return {
        "label": label,
        "rc": proc.returncode,
        "elapsed_sec": dt,
        "json": parsed,
        "stopped_reason": stopped_reason,
        "hard_stop": hard_stop,
    }


def main():
    p = argparse.ArgumentParser(description="One-shot daily runner: Microsoft Rewards + JD (~4.5 min)")
    p.add_argument("--host", default=DEFAULT_HOST)
    p.add_argument("--adb", default=DEFAULT_ADB)
    p.add_argument("--skip-ms", action="store_true")
    p.add_argument("--skip-jd", action="store_true")
    p.add_argument("--search-interval", type=float, default=6.0)
    p.add_argument("--jd-browse-sec", type=float, default=8.5)
    args = p.parse_args()

    t_start = time.monotonic()
    common = ["--host", args.host, "--adb", args.adb]
    results = []
    aborted_reason = None

    def check_abort(step_res: dict) -> bool:
        nonlocal aborted_reason
        if step_res.get("hard_stop"):
            aborted_reason = f"Stopped safely after '{step_res['label']}' due to guard/CAPTCHA ({step_res.get('stopped_reason') or 'rc=' + str(step_res['rc'])})"
            print(f"[STOP] {aborted_reason}", file=sys.stderr, flush=True)
            return True
        return False

    if not args.skip_ms:
        r_edge = run_step("MS Edge CDP", [sys.executable, str(REWARDS_RUNNER), *common, "edge-all", "--search-interval", str(args.search_interval)])
        results.append(r_edge)
        if r_edge["rc"] != 0:
            print(f"[WARN] MS Edge step exited {r_edge['rc']}", flush=True)
        if not check_abort(r_edge):
            r_ci = run_step("Bing Check-in", [sys.executable, str(REWARDS_RUNNER), *common, "bing-checkin"])
            results.append(r_ci)
            if not check_abort(r_ci):
                # Skip bing-read automatically if 0-screenshot DOM already shows Read-to-Earn 30 or 30/30
                read_done = False
                try:
                    rd_val = str(((r_ci.get("json") or {}).get("after") or {}).get("readToEarn") or "").strip()
                    if re.match(r"^30(?:/30)?$", rd_val):
                        read_done = True
                        print(f"[INFO] Bing Read-to-Earn already {rd_val} per DOM; skipping bing-read.", flush=True)
                except Exception:
                    pass
                if not read_done:
                    r_rd = run_step("Bing Read 30/30", [sys.executable, str(REWARDS_RUNNER), *common, "bing-read", "--count", "10"])
                    results.append(r_rd)
                    check_abort(r_rd)

    if not args.skip_jd and not aborted_reason:
        r_farm = run_step(
            "JD 东东农场",
            [sys.executable, str(JD_RUNNER), *common, "farm-all", "--max-browse", "24", "--browse-sec", str(args.jd_browse_sec)],
        )
        results.append(r_farm)
        if not check_abort(r_farm):
            r_bean = run_step("JD 领京豆刮卡", [sys.executable, str(JD_RUNNER), *common, "bean-signin"])
            results.append(r_bean)
            check_abort(r_bean)

    failed = bool(aborted_reason) or any(r["rc"] != 0 or bool(r.get("stopped_reason")) for r in results)
    summary = {
        "ok": not failed,
        "aborted_reason": aborted_reason,
        "total_elapsed_sec": round(time.monotonic() - t_start, 1),
        "steps": [{k: v for k, v in r.items() if k != "json"} for r in results],
    }
    print("\n=== SUMMARY ===", flush=True)
    print(json.dumps(summary, ensure_ascii=False, indent=2))
    if failed:
        sys.exit(1)


if __name__ == "__main__":
    main()
