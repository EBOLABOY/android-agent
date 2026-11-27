#!/system/bin/sh
MODDIR="${0%/*}"
DATA_DIR="/data/adb/android_agent"
RUN_DIR="$DATA_DIR/run"
BIN="$MODDIR/bin/agentd"
CONF="$DATA_DIR/config.json"
LOG="$DATA_DIR/agentd.log"
WATCHDOG_PID="$RUN_DIR/watchdog.pid"
DAEMON_PID="$RUN_DIR/agentd.pid"

is_pid_matching() {
  _pid="$1"
  _pat="$2"
  case "$_pid" in
    ''|*[!0-9]*) return 1 ;;
  esac
  [ -r "/proc/$_pid/cmdline" ] || return 1
  kill -0 "$_pid" 2>/dev/null || return 1
  tr '\0' ' ' < "/proc/$_pid/cmdline" 2>/dev/null | grep -qE "$_pat"
}

mkdir -p "$DATA_DIR" "$RUN_DIR"
chmod 700 "$DATA_DIR" "$RUN_DIR"
rm -rf /dev/android_agent 2>/dev/null || true
rm -f "$DATA_DIR/watchdog.pid" "$DATA_DIR/agentd.pid" 2>/dev/null || true

# Prevent duplicate watchdog instances. Because RUN_DIR persists across reboots,
# strictly verify /proc/$PID/cmdline (excluding our own $$ and $PPID to prevent
# reboot self-PID collision) and purge any stale PID file so reboot never stalls.
if [ -f "$WATCHDOG_PID" ]; then
  OLD_WPID="$(cat "$WATCHDOG_PID" 2>/dev/null)"
  if [ "$OLD_WPID" != "$$" ] && [ "$OLD_WPID" != "$PPID" ] && is_pid_matching "$OLD_WPID" 'android_agent.*service\.sh'; then
    exit 0
  fi
  rm -f "$WATCHDOG_PID"
fi

# Strictly validate persisted agentd.pid: remove stale post-reboot PID files,
# and terminate any orphaned agentd left behind without a live watchdog.
if [ -f "$DAEMON_PID" ]; then
  OLD_DPID="$(cat "$DAEMON_PID" 2>/dev/null)"
  if is_pid_matching "$OLD_DPID" 'android_agent.*agentd|agentd.*android_agent'; then
    kill "$OLD_DPID" 2>/dev/null || true
    sleep 1
  fi
  rm -f "$DAEMON_PID"
fi

(
  exec </dev/null >/dev/null 2>&1

  # Wait for Android boot completion
  while [ "$(getprop sys.boot_completed)" != "1" ]; do
    sleep 2
  done
  sleep 2

  BACKOFF=3
  STDERR_LOG="$DATA_DIR/agentd.stderr.log"

  while true; do
    if [ -f "$MODDIR/disable" ] || [ -f "$MODDIR/remove" ] || [ -f "$DATA_DIR/stop" ]; then
      if [ -f "$DAEMON_PID" ]; then
        DPID="$(cat "$DAEMON_PID" 2>/dev/null)"
        if is_pid_matching "$DPID" 'android_agent.*agentd|agentd.*android_agent'; then
          kill "$DPID" 2>/dev/null || true
        fi
      fi
      rm -f "$WATCHDOG_PID" "$DAEMON_PID"
      exit 0
    fi

    # Keep boot/stderr logs bounded
    if [ -f "$LOG" ]; then
      LOG_SZ="$(wc -c < "$LOG" 2>/dev/null || echo 0)"
      if [ "$LOG_SZ" -gt 2097152 ]; then
        mv -f "$LOG" "${LOG}.1" 2>/dev/null || true
      fi
    fi
    if [ -f "$STDERR_LOG" ]; then
      ERR_SZ="$(wc -c < "$STDERR_LOG" 2>/dev/null || echo 0)"
      if [ "$ERR_SZ" -gt 65536 ]; then
        mv -f "$STDERR_LOG" "${STDERR_LOG}.1" 2>/dev/null || true
      fi
    fi

    if [ ! -x "$BIN" ]; then
      chmod 755 "$BIN" 2>/dev/null || true
    fi

    START_TS="$(date +%s 2>/dev/null || echo 0)"
    echo "$(date '+%Y-%m-%d %H:%M:%S') [watchdog] starting agentd (backoff=${BACKOFF}s)" >> "$LOG"
    "$BIN" -config "$CONF" -log-file "$LOG" >/dev/null 2>>"$STDERR_LOG" &
    CHILD=$!
    echo "$CHILD" > "$DAEMON_PID"
    wait "$CHILD"
    RC=$?
    END_TS="$(date +%s 2>/dev/null || echo 0)"
    RAN_SEC=$((END_TS - START_TS))
    echo "$(date '+%Y-%m-%d %H:%M:%S') [watchdog] agentd exited with code $RC after ${RAN_SEC}s" >> "$LOG"
    rm -f "$DAEMON_PID"

    if [ -f "$MODDIR/disable" ] || [ -f "$MODDIR/remove" ] || [ -f "$DATA_DIR/stop" ]; then
      rm -f "$WATCHDOG_PID"
      exit 0
    fi

    if [ "$RAN_SEC" -ge 15 ]; then
      BACKOFF=3
    else
      case "$BACKOFF" in
        3) BACKOFF=6 ;;
        6) BACKOFF=15 ;;
        15) BACKOFF=30 ;;
        *) BACKOFF=60 ;;
      esac
    fi
    sleep "$BACKOFF"
  done
) </dev/null >/dev/null 2>&1 &
echo $! > "$WATCHDOG_PID"
