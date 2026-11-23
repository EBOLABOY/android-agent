#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO_BIN="${GO_BIN:-$(command -v go 2>/dev/null || echo /root/router-dashboard/go/bin/go)}"
SMALI_JAR="${SMALI_JAR:-/usr/share/java/smali.jar}"

echo "==> [1/4] Assembling cliphelper.jar from AgentClip.smali"
mkdir -p "$ROOT_DIR/build/clip" "$ROOT_DIR/internal/backend" "$ROOT_DIR/ksu-module/bin"
java -jar "$SMALI_JAR" assemble -a 34 -o "$ROOT_DIR/build/clip/classes.dex" "$ROOT_DIR/cliphelper/AgentClip.smali"
(cd "$ROOT_DIR/build/clip" && jar cf "$ROOT_DIR/internal/backend/cliphelper.jar" classes.dex)

echo "==> [2/4] Running Go unit & integration tests on host"
(cd "$ROOT_DIR" && "$GO_BIN" test -v -race ./...)

echo "==> [3/4] Cross-compiling static linux/arm64 binary"
(
  cd "$ROOT_DIR"
  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 "$GO_BIN" build \
    -trimpath \
    -ldflags="-s -w" \
    -o "$ROOT_DIR/ksu-module/bin/agentd" \
    ./cmd/agentd
)

chmod 755 "$ROOT_DIR/ksu-module/bin/agentd" \
          "$ROOT_DIR/ksu-module/service.sh" \
          "$ROOT_DIR/ksu-module/uninstall.sh" \
          "$ROOT_DIR/ksu-module/action.sh"

echo "==> [4/4] Verifying ARM64 static ELF"
file "$ROOT_DIR/ksu-module/bin/agentd"
ls -lh "$ROOT_DIR/ksu-module/bin/agentd"
if command -v readelf >/dev/null 2>&1; then
  readelf -h "$ROOT_DIR/ksu-module/bin/agentd" | grep -E 'Class:|Machine:|Type:' || true
  readelf -l "$ROOT_DIR/ksu-module/bin/agentd" | grep -q 'INTERP' && echo "WARNING: dynamically linked!" || echo "Static ELF confirmed (no PT_INTERP)."
fi
