#!/system/bin/sh
# Bridge for KernelSU Manager WebUI (https://mui.kernelsu.org) -> local agentd (127.0.0.1:8765)
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
  *..*|*[[:space:]]*)
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

URL="http://127.0.0.1:8765$REQ_PATH"

if [ -n "$BODY_B64" ]; then
  RESP="$(printf '%s' "$BODY_B64" | base64 -d 2>/dev/null | /system/bin/curl --path-as-is -sS --connect-timeout 5 --max-time 85 -X "$METHOD" \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    --data-binary @- \
    -w "\n%{http_code}" "$URL" 2>/dev/null)"
else
  RESP="$(/system/bin/curl --path-as-is -sS --connect-timeout 5 --max-time 85 -X "$METHOD" \
    -H "Authorization: Bearer $TOKEN" \
    -w "\n%{http_code}" "$URL" 2>/dev/null)"
fi
CURL_RC=$?

if [ "$CURL_RC" -ne 0 ] || [ -z "$RESP" ]; then
  printf '%s\n' '{"error":"无法连接 agentd (127.0.0.1:8765)，可能处于王者游戏守护暂停中或未启动"}'
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
