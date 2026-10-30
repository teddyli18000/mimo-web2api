#!/usr/bin/env bash
# 打包浏览器扩展。CI 与 release 共用同一份逻辑，避免两条流水线各写一遍再漂移。
#
# 用法: bash .github/scripts/pack-extension.sh <version> [out-dir]
# 产出: <out-dir>/mimo-web2api-extension-<version>.zip
#       zip 内是单层目录 mimo-web2api-extension/，解压后可直接「加载已解压的扩展程序」
set -euo pipefail

VERSION="${1:-}"
OUT_DIR="${2:-.}"

if [ -z "$VERSION" ]; then
  echo "usage: $0 <version> [out-dir]" >&2
  exit 2
fi

# Chrome 只接受 1~4 段纯整数的 version，别把 tag 里的日期/后缀塞进去
if ! [[ "$VERSION" =~ ^[0-9]+(\.[0-9]+){1,3}$ ]]; then
  echo "::error::invalid extension version: $VERSION (expect 1-4 dot-separated integers)" >&2
  exit 1
fi

ROOT=$(cd "$(dirname "$0")/.." && pwd)
mkdir -p "$OUT_DIR"
OUT_DIR=$(cd "$OUT_DIR" && pwd)

STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT

mkdir -p "$STAGE/mimo-web2api-extension"
cp -r "$ROOT/browser-extension/." "$STAGE/mimo-web2api-extension/"

# 仓库里的 manifest 版本是基线值，发行时跟着 tag 走
jq --arg v "$VERSION" '.version = $v' \
  "$STAGE/mimo-web2api-extension/manifest.json" > "$STAGE/manifest.json.tmp"
mv "$STAGE/manifest.json.tmp" "$STAGE/mimo-web2api-extension/manifest.json"

ZIP="$OUT_DIR/mimo-web2api-extension-$VERSION.zip"
rm -f "$ZIP"
(cd "$STAGE" && zip -qr "$ZIP" mimo-web2api-extension)

# 解压后根目录必须是一个文件夹，且七件套齐全：路径写错会产出「散落一地」的坏包
LIST=$(unzip -Z1 "$ZIP")
for f in manifest.json popup.html popup.js \
         icons/icon16.png icons/icon32.png icons/icon48.png icons/icon128.png; do
  if ! printf '%s\n' "$LIST" | grep -qx "mimo-web2api-extension/$f"; then
    echo "::error::missing mimo-web2api-extension/$f in $ZIP" >&2
    exit 1
  fi
done

VERSION="$VERSION" python3 - "$STAGE/mimo-web2api-extension/manifest.json" <<'PY'
import json, os, sys

m = json.load(open(sys.argv[1], encoding="utf-8"))
assert m["version"] == os.environ["VERSION"], f"version not stamped: {m['version']}"
assert m["manifest_version"] == 3, "manifest_version must be 3"
for key in ("icons", "action"):
    assert key in m, f"manifest missing {key}"
print(f"packed {m['name']} {m['version']}")
PY

echo "artifact: $ZIP"
