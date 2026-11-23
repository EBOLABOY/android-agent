#!/system/bin/sh
DATA_DIR="/data/adb/android_agent"
RUN_DIR="/dev/android_agent"
DAEMON_PID="$RUN_DIR/agentd.pid"

if [ -f "$DAEMON_PID" ]; then
  PID="$(cat "$DAEMON_PID" 2>/dev/null)"
  if [ -n "$PID" ] && kill -0 "$PID" 2>/dev/null; then
    echo "android_agent running (PID=$PID)"
    echo "Config: $DATA_DIR/config.json"
    echo "Log: $DATA_DIR/agentd.log"
    exit 0
  fi
fi

echo "android_agent is NOT running; starting via service.sh..."
rm -f "$DATA_DIR/stop"
sh "${0%/*}/service.sh"
sleep 3
if [ -f "$DAEMON_PID" ]; then
  echo "started (PID=$(cat "$DAEMON_PID"))"
fi
