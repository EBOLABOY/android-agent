#!/system/bin/sh
DATA_DIR="/data/adb/android_agent"
RUN_DIR="/dev/android_agent"
touch "$DATA_DIR/stop" 2>/dev/null || true

for pf in "$RUN_DIR/watchdog.pid" "$RUN_DIR/agentd.pid" "$DATA_DIR/watchdog.pid" "$DATA_DIR/agentd.pid"; do
  if [ -f "$pf" ]; then
    PID="$(cat "$pf" 2>/dev/null)"
    [ -n "$PID" ] && kill "$PID" 2>/dev/null || true
    rm -f "$pf"
  fi
done

rm -rf "$RUN_DIR" 2>/dev/null || true
rm -f /data/local/tmp/android_agent_cliphelper.jar 2>/dev/null || true
