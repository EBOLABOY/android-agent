#!/system/bin/sh
DATA_DIR="/data/adb/android_agent"
RUN_DIR="$DATA_DIR/run"
DAEMON_PID="$RUN_DIR/agentd.pid"

is_agentd_pid() {
  _pid="$1"
  case "$_pid" in
    ''|*[!0-9]*) return 1 ;;
  esac
  [ -r "/proc/$_pid/cmdline" ] || return 1
  kill -0 "$_pid" 2>/dev/null || return 1
  tr '\0' ' ' < "/proc/$_pid/cmdline" 2>/dev/null | grep -qE 'android_agent.*agentd|agentd.*android_agent'
}

if [ -f "$DAEMON_PID" ]; then
  PID="$(cat "$DAEMON_PID" 2>/dev/null)"
  if is_agentd_pid "$PID"; then
    echo "android_agent running (PID=$PID)"
    echo "Config: $DATA_DIR/config.json"
    echo "Log: $DATA_DIR/agentd.log"
    exit 0
  fi
  rm -f "$DAEMON_PID"
fi

echo "android_agent is NOT running; starting via service.sh..."
rm -f "$DATA_DIR/stop"
sh "${0%/*}/service.sh"
sleep 3
if [ -f "$DAEMON_PID" ]; then
  PID="$(cat "$DAEMON_PID" 2>/dev/null)"
  if is_agentd_pid "$PID"; then
    echo "started (PID=$PID)"
  fi
fi
