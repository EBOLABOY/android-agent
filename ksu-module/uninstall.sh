#!/system/bin/sh
DATA_DIR="/data/adb/android_agent"
RUN_DIR="$DATA_DIR/run"
touch "$DATA_DIR/stop" 2>/dev/null || true

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

for pf in "$RUN_DIR/watchdog.pid" "/dev/android_agent/watchdog.pid" "$DATA_DIR/watchdog.pid"; do
  if [ -f "$pf" ]; then
    PID="$(cat "$pf" 2>/dev/null)"
    if is_pid_matching "$PID" 'android_agent.*service\.sh'; then
      kill "$PID" 2>/dev/null || true
    fi
    rm -f "$pf"
  fi
done

for pf in "$RUN_DIR/agentd.pid" "/dev/android_agent/agentd.pid" "$DATA_DIR/agentd.pid"; do
  if [ -f "$pf" ]; then
    PID="$(cat "$pf" 2>/dev/null)"
    if is_pid_matching "$PID" 'android_agent.*agentd|agentd.*android_agent'; then
      kill "$PID" 2>/dev/null || true
    fi
    rm -f "$pf"
  fi
done

rm -rf "$RUN_DIR" /dev/android_agent 2>/dev/null || true
rm -rf /data/local/tmp/.ag_cache 2>/dev/null || true
rm -f /data/local/tmp/android_agent_cliphelper.jar 2>/dev/null || true
