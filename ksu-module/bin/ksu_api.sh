#!/system/bin/sh
# Bridge for KernelSU Manager WebUI (https://mui.kernelsu.org) -> local agentd
set -u

METHOD="${1:-}"
REQ_PATH="${2:-}"
BODY_B64="${3:-}"
CONF="/data/adb/android_agent/config.json"

case "$METHOD" in
  GET|POST) ;;
  *)
    printf '%s\n' '{"error":"unsupported HTTP method"}'
    exit 2
    ;;
esac

case "$REQ_PATH" in
  /api/v1/*|/health) ;;
  *)
    printf '%s\n' '{"error":"forbidden API path"}'
    exit 2
    ;;
esac

case "$REQ_PATH" in
  *..*|*%2e%2e*|*%2E%2E*|*%2e%2E*|*%2E%2e*|*\#*|*[[:space:]]*)
    printf '%s\n' '{"error":"invalid API path"}'
    exit 2
    ;;
esac

if [ ! -r "$CONF" ]; then
  printf '%s\n' '{"error":"config.json unreadable"}'
  exit 1
fi

TOKEN="$(grep '"bearer_token"' "$CONF" | head -n 1 | sed -E 's/.*"bearer_token"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')"
if [ -z "$TOKEN" ]; then
  printf '%s\n' '{"error":"bearer_token missing in config.json"}'
  exit 1
fi
case "$TOKEN" in
  *\"*|*\\*)
    printf '%s\n' '{"error":"invalid bearer_token format"}'
    exit 1
    ;;
esac

LISTEN_ADDR="$(grep '"listen_addr"' "$CONF" | head -n 1 | sed -E 's/.*"listen_addr"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/')"
case "$LISTEN_ADDR" in
  ""|*[![:alnum:].:_-]*)
    LISTEN_ADDR="127.0.0.1:8765"
    ;;
  0.0.0.0:*)
    LISTEN_ADDR="127.0.0.1:${LISTEN_ADDR#0.0.0.0:}"
    ;;
  :*)
    LISTEN_ADDR="127.0.0.1${LISTEN_ADDR}"
    ;;
esac

MAX_TIME=25
case "$REQ_PATH" in
  /api/v1/chat|/api/v1/chat\?*|/api/v1/chat/stream*)
    MAX_TIME=85
    ;;
esac

URL="http://${LISTEN_ADDR}${REQ_PATH}"
TMP_BODY=""
cleanup() {
  if [ -n "$TMP_BODY" ]; then
    rm -f "$TMP_BODY"
  fi
}
trap cleanup EXIT INT TERM

if [ -n "$BODY_B64" ]; then
  TMP_DIR="${TMPDIR:-/data/local/tmp}"
  if [ ! -d "$TMP_DIR" ] || [ ! -w "$TMP_DIR" ]; then
    TMP_DIR="/tmp"
  fi
  OLD_UMASK="$(umask)"
  umask 077
  TMP_BODY="${TMP_DIR}/.ksu_api_body.$$"
  if ! printf '%s' "$BODY_B64" | base64 -d > "$TMP_BODY" 2>/dev/null; then
    umask "$OLD_UMASK"
    printf '%s\n' '{"error":"invalid base64 request body"}'
    exit 2
  fi
  umask "$OLD_UMASK"
  RESP="$(printf 'header = "Authorization: Bearer %s"\n' "$TOKEN" | \
    /system/bin/curl --path-as-is -sS --connect-timeout 5 --max-time "$MAX_TIME" \
      -K - -X "$METHOD" \
      -H "Content-Type: application/json" \
      --data-binary "@$TMP_BODY" \
      -w "\n%{http_code}" -- "$URL" 2>/dev/null)"
else
  RESP="$(printf 'header = "Authorization: Bearer %s"\n' "$TOKEN" | \
    /system/bin/curl --path-as-is -sS --connect-timeout 5 --max-time "$MAX_TIME" \
      -K - -X "$METHOD" \
      -w "\n%{http_code}" -- "$URL" 2>/dev/null)"
fi
CURL_RC=$?

if [ "$CURL_RC" -ne 0 ] || [ -z "$RESP" ]; then
  printf '{"error":"无法连接 agentd (%s)，可能处于王者游戏守护暂停中或未启动"}\n' "$LISTEN_ADDR"
  exit 1
fi

NL="$(printf '\nx')"
NL="${NL%x}"
HTTP_CODE="${RESP##*"$NL"}"
BODY="${RESP%"$NL"*}"
if [ "$BODY" = "$RESP" ]; then
  BODY="{}"
fi

printf '%s\n' "$BODY"
case "$HTTP_CODE" in
  2[0-9][0-9]) exit 0 ;;
  *) exit 1 ;;
esac
