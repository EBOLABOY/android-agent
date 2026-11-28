#!/system/bin/sh
# Fast Bridge (<15ms) for KernelSU Manager WebUI -> local agentd
set -u

METHOD="${1:-}"
REQ_PATH="${2:-}"
BODY_B64="${3:-}"
CONF="${KSU_AGENT_CONF:-/data/adb/android_agent/config.json}"

case "$METHOD" in
  GET|POST) ;;
  *)
    printf '%s\n' '{"error":"unsupported HTTP method"}'
    exit 2
    ;;
esac

case "$REQ_PATH" in
  /api/v1/*|/health|/health\?*) ;;
  *)
    printf '%s\n' '{"error":"forbidden API path"}'
    exit 2
    ;;
esac

case "$REQ_PATH" in
  *..*|*%2e%2e*|*%2E%2E*|*%2e%2E*|*%2E%2e*|*\#*|*[[:space:]]*|*[[:cntrl:]]*)
    printf '%s\n' '{"error":"invalid API path"}'
    exit 2
    ;;
esac

if [ ! -r "$CONF" ]; then
  printf '%s\n' '{"error":"config.json unreadable"}'
  exit 1
fi

# Zero-fork pure shell JSON key extraction (handles both pretty-printed and single-line JSON)
TOKEN=""
LISTEN_ADDR=""
while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in
    *'"bearer_token"'*)
      if [ -z "$TOKEN" ]; then
        rest="${line#*\"bearer_token\"}"
        case "$rest" in
          *:*)
            rest="${rest#*:}"
            while :; do
              case "$rest" in
                " "*|"	"*) rest="${rest#?}" ;;
                *) break ;;
              esac
            done
            case "$rest" in
              \"*\"*)
                rest="${rest#\"}"
                TOKEN="${rest%%\"*}"
                ;;
            esac
            ;;
        esac
      fi
      ;;
  esac
  case "$line" in
    *'"listen_addr"'*)
      if [ -z "$LISTEN_ADDR" ]; then
        rest="${line#*\"listen_addr\"}"
        case "$rest" in
          *:*)
            rest="${rest#*:}"
            while :; do
              case "$rest" in
                " "*|"	"*) rest="${rest#?}" ;;
                *) break ;;
              esac
            done
            case "$rest" in
              \"*\"*)
                rest="${rest#\"}"
                LISTEN_ADDR="${rest%%\"*}"
                ;;
            esac
            ;;
        esac
      fi
      ;;
  esac
  if [ -n "$TOKEN" ] && [ -n "$LISTEN_ADDR" ]; then
    break
  fi
done < "$CONF"

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

MAX_TIME=20
case "$REQ_PATH" in
  /api/v1/chat|/api/v1/chat\?*|/api/v1/chat/stream*)
    MAX_TIME=85
    ;;
  /api/v1/chat/events*)
    # Supports ?wait_ms=8000..12000 long-polling cleanly
    MAX_TIME=20
    ;;
  /health*|/api/v1/status*|/api/v1/bootstrap*)
    MAX_TIME=12
    ;;
esac

URL="http://${LISTEN_ADDR}${REQ_PATH}"
CURL_BIN="/system/bin/curl"
[ -x "$CURL_BIN" ] || CURL_BIN="curl"
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
    "$CURL_BIN" --path-as-is -sS --connect-timeout 2 --max-time "$MAX_TIME" \
      -K - -X "$METHOD" \
      -H "Content-Type: application/json" \
      --data-binary "@$TMP_BODY" \
      -w "\n%{http_code}" -- "$URL" 2>/dev/null)"
else
  RESP="$(printf 'header = "Authorization: Bearer %s"\n' "$TOKEN" | \
    "$CURL_BIN" --path-as-is -sS --connect-timeout 2 --max-time "$MAX_TIME" \
      -K - -X "$METHOD" \
      -w "\n%{http_code}" -- "$URL" 2>/dev/null)"
fi
CURL_RC=$?

if [ "$CURL_RC" -ne 0 ] || [ -z "$RESP" ]; then
  printf '{"error":"无法连接 agentd (%s)，可能处于王者游戏守护暂停中或未启动"}\n' "$LISTEN_ADDR"
  exit 1
fi

NL='
'
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
