#!/usr/bin/env bash
# 打发布包：linux/amd64 与 linux/arm64 的 tar.gz + SHA256SUMS
#
#   scripts/build-release.sh v1.0.0        # 产物在 dist/
#   ARCHES="amd64" scripts/build-release.sh v1.0.0
#
# 包里 = lightcat 二进制（另有 acs 硬链接，兼容老 update.sh 按名找二进制）
#        + install.sh / uninstall.sh / update.sh + lightcat.service + README + LICENSE + VERSION
#
# 每个架构出**两个同内容的包**：lightcat-*（正名）与 acs-*（兼容名）。
# 为什么要兼容名：≤v1.2.x 的安装里 update.sh 按 acs-<版本>-linux-<架构>.tar.gz
# 下载资产、按 -name acs 找二进制 —— 只发新名的话，存量安装的
# 「sudo ./update.sh」会当场断掉。发布时两个包都传上 release 即可。
set -euo pipefail

cd "$(dirname "$0")/.."
ROOT=$(pwd)

VERSION=${1:-}
if [ -z "$VERSION" ]; then
  VERSION=$(git describe --tags --abbrev=0 2>/dev/null || true)
fi
if [ -z "$VERSION" ]; then
  echo "用法: scripts/build-release.sh <版本，如 v1.0.0>" >&2
  exit 1
fi
VER=${VERSION#v}          # 文件名里不带 v
ARCHES=${ARCHES:-"amd64 arm64"}

if ! command -v go >/dev/null 2>&1; then
  export PATH="$HOME/.local/go/bin:$PATH"
fi
command -v go >/dev/null 2>&1 || { echo "找不到 go" >&2; exit 1; }

DIST="$ROOT/dist"
STAGE="$DIST/stage"
rm -rf "$STAGE"
mkdir -p "$STAGE"

# 清掉本版本可能残留的旧包，免得它们混进 SHA256SUMS
BUILT=""
for arch in $ARCHES; do
  rm -f "$DIST/lightcat-${VER}-linux-${arch}.tar.gz" "$DIST/acs-${VER}-linux-${arch}.tar.gz"
done

for arch in $ARCHES; do
  pkg="lightcat-${VER}-linux-${arch}"
  out="$STAGE/$pkg"
  mkdir -p "$out"
  echo "== 编译 $pkg"
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
    go build -trimpath -ldflags "-s -w" -o "$out/lightcat" ./cmd/lightcat
  # 兼容硬链接：老 update.sh 在包里按 -name acs 找二进制（同一 inode，不占双份空间；
  # 跨到 tar 里也是硬链接条目）
  ln "$out/lightcat" "$out/acs"

  install -m 0755 deploy/install.sh   "$out/install.sh"
  install -m 0755 deploy/uninstall.sh "$out/uninstall.sh"
  install -m 0755 deploy/update.sh    "$out/update.sh"
  install -m 0644 deploy/lightcat.service  "$out/lightcat.service"
  install -m 0644 deploy/README.md    "$out/README.md"
  install -m 0644 LICENSE             "$out/LICENSE"
  printf '%s\n' "$VER" > "$out/VERSION"

  tar czf "$DIST/$pkg.tar.gz" -C "$STAGE" "$pkg"
  BUILT="$BUILT $pkg.tar.gz"
  # 兼容包名的副本（内容一致，见文件头说明）
  cp "$DIST/$pkg.tar.gz" "$DIST/acs-${VER}-linux-${arch}.tar.gz"
  BUILT="$BUILT acs-${VER}-linux-${arch}.tar.gz"
  rm -rf "$out"
  echo "   → dist/$pkg.tar.gz + dist/acs-${VER}-linux-${arch}.tar.gz（兼容名，$(du -h "$DIST/$pkg.tar.gz" | cut -f1)）"
done

rmdir "$STAGE" 2>/dev/null || true

# 校验和（update.sh 会拿它验证）；只列本次产出的包
( cd "$DIST" && sha256sum $BUILT > SHA256SUMS )
echo "== SHA256SUMS"
cat "$DIST/SHA256SUMS"
echo "== 内容示例"
first_arch=$(echo $ARCHES | awk '{print $1}')
tar tzf "$DIST/lightcat-${VER}-linux-${first_arch}.tar.gz" | sed 's/^/   /'
