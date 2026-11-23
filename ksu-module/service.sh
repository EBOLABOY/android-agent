#!/system/bin/sh
MODDIR="${0%/*}"
DATA_DIR="/data/adb/android_agent"
RUN_DIR="/dev/android_agent"
BIN="$MODDIR/bin/agentd"
CONF="$DATA_DIR/config.json"
LOG="$DATA_DIR/agentd.log"
WATCHDOG_PID="$RUN_DIR/watchdog.pid"
DAEMON_PID="$RUN_DIR/agentd.pid"

mkdir -p "$DATA_DIR" "$RUN_DIR"
chmod 700 "$DATA_DIR" "$RUN_DIR"
rm -f "$DATA_DIR/watchdog.pid" "$DATA_DIR/agentd.pid" 2>/dev/null || true

# Prevent duplicate watchdog instances (tmpfs PID dir + /proc cmdline check avoids stale PID collision)
if [ -f "$WATCHDOG_PID" ]; then
  OLD_WPID="$(cat "$WATCHDOG_PID" 2>/dev/null)"
  if [ -n "$OLD_WPID" ] && kill -0 "$OLD_WPID" 2>/dev/null; then
    if tr '\0' ' ' < "/proc/$OLD_WPID/cmdline" 2>/dev/null | grep -q 'android_agent'; then
      exit 0
    fi
  fi
fi

(
  exec </dev/null >/dev/null 2>&1

  # Wait for Android boot completion
  while [ "$(getprop sys.boot_completed)" != "1" ]; do
    sleep 2
  done
  sleep 2

  while true; do
    if [ -f "$MODDIR/disable" ] || [ -f "$MODDIR/remove" ] || [ -f "$DATA_DIR/stop" ]; then
      if [ -f "$DAEMON_PID" ]; then
        DPID="$(cat "$DAEMON_PID" 2>/dev/null)"
        [ -n "$DPID" ] && kill "$DPID" 2>/dev/null || true
      fi
      rm -f "$WATCHDOG_PID" "$DAEMON_PID"
      exit 0
    fi

    # Keep log bounded (~2MB)
    if [ -f "$LOG" ]; then
      LOG_SZ="$(wc -c < "$LOG" 2>/dev/null || echo 0)"
      if [ "$LOG_SZ" -gt 2097152 ]; then
        mv -f "$LOG" "${LOG}.old" 2>/dev/null || true
      fi
    fi

    if [ ! -x "$BIN" ]; then
      chmod 755 "$BIN" 2>/dev/null || true
    fi

    echo "$(date '+%Y-%m-%d %H:%M:%S') [watchdog] starting agentd" >> "$LOG"
    "$BIN" -config "$CONF" >> "$LOG" 2>&1 &
    CHILD=$!
    echo "$CHILD" > "$DAEMON_PID"
    wait "$CHILD"
    RC=$?
    echo "$(date '+%Y-%m-%d %H:%M:%S') [watchdog] agentd exited with code $RC" >> "$LOG"
    rm -f "$DAEMON_PID"

    if [ -f "$MODDIR/disable" ] || [ -f "$MODDIR/remove" ] || [ -f "$DATA_DIR/stop" ]; then
      rm -f "$WATCHDOG_PID"
      exit 0
    fi
    sleep 3
  done
) </dev/null >/dev/null 2>&1 &
echo $! > "$WATCHDOG_PID"
