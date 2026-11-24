#!/usr/bin/env bash
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT_DIR="$DIR/build"
FRAMEWORK_RES="${FRAMEWORK_RES:-/usr/share/android-framework-res/framework-res.apk}"
SMALI_JAR="${SMALI_JAR:-/usr/share/java/smali.jar}"
KEYSTORE="$DIR/debug.keystore"
FINAL_APK="$OUT_DIR/agent-webview-debug.apk"

rm -rf "$OUT_DIR"
mkdir -p "$OUT_DIR/res_flat" "$OUT_DIR/dex"

echo "==> [1/5] Assembling Smali -> classes.dex"
java -jar "$SMALI_JAR" assemble -a 34 -o "$OUT_DIR/dex/classes.dex" "$DIR/smali"

echo "==> [2/5] Compiling resources & linking base APK with aapt2"
aapt2 compile --dir "$DIR/res" -o "$OUT_DIR/res_flat/res.zip"
aapt2 link \
  -I "$FRAMEWORK_RES" \
  --manifest "$DIR/AndroidManifest.xml" \
  -A "$DIR/assets" \
  -o "$OUT_DIR/unsigned.apk" \
  "$OUT_DIR/res_flat/res.zip"

echo "==> [3/5] Injecting classes.dex into APK"
python3 - "$OUT_DIR/unsigned.apk" "$OUT_DIR/dex/classes.dex" <<'PYEOF'
import sys, zipfile
with zipfile.ZipFile(sys.argv[1], "a", compression=zipfile.ZIP_DEFLATED) as zf:
    zf.write(sys.argv[2], "classes.dex")
PYEOF

echo "==> [4/5] Aligning APK with zipalign"
zipalign -f -p 4 "$OUT_DIR/unsigned.apk" "$OUT_DIR/aligned.apk"

echo "==> [5/5] Signing APK (v1/v2/v3) with apksigner"
if [ ! -f "$KEYSTORE" ]; then
  keytool -genkeypair -v \
    -keystore "$KEYSTORE" \
    -storepass android \
    -alias agentdebug \
    -keypass android \
    -keyalg RSA \
    -keysize 2048 \
    -validity 10000 \
    -dname "CN=AgentWebViewDebug, OU=Local, O=Local, L=Local, S=Local, C=CN" >/dev/null 2>&1
fi

apksigner sign \
  --ks "$KEYSTORE" \
  --ks-pass pass:android \
  --key-pass pass:android \
  --ks-key-alias agentdebug \
  --out "$FINAL_APK" \
  "$OUT_DIR/aligned.apk"

apksigner verify --verbose "$FINAL_APK" | head -n 12
ls -lh "$FINAL_APK"
