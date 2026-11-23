#!/usr/bin/env bash
# 打包 .deb（amd64 + arm64）
#   VERSION=1.1.0 ARCHS="amd64 arm64" bash packaging/build-deb.sh
# 依赖：先交叉编译 helper 到 dist/homing-helper-<arch>（见仓库根 README）
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/.." && pwd)
VERSION=${VERSION:-2.1.0}
DIST=${DIST:-$ROOT/dist}
ARCHS=${ARCHS:-"amd64 arm64"}

need() { [ -e "$1" ] || { echo "缺少 $1" >&2; exit 1; }; }
need "$ROOT/gui/homing_gui.py"
need "$ROOT/packaging/control.in"

mkdir -p "$DIST"

for ARCH in $ARCHS; do
  HELPER="$DIST/homing-helper-$ARCH"
  need "$HELPER"
  TREE="$DIST/pkg-$ARCH"
  rm -rf "$TREE"
  install -d "$TREE/DEBIAN" \
    "$TREE/usr/bin" "$TREE/usr/libexec/homing" \
    "$TREE/usr/share/homing/gui" "$TREE/usr/share/homing/examples" \
    "$TREE/usr/share/applications" \
    "$TREE/usr/share/icons/hicolor/scalable/apps" \
    "$TREE/usr/share/polkit-1/actions" \
    "$TREE/usr/share/doc/homing-client"

  install -m 0755 "$HELPER"                                "$TREE/usr/libexec/homing/homing-helper"
  install -m 0755 "$ROOT/packaging/homing"                   "$TREE/usr/bin/homing"
  install -m 0755 "$ROOT/packaging/homing-gui"               "$TREE/usr/bin/homing-gui"
  install -m 0644 "$ROOT/gui/homing_gui.py"                  "$TREE/usr/share/homing/gui/homing_gui.py"
  install -m 0644 "$ROOT/packaging/homing.desktop"     "$TREE/usr/share/applications/homing.desktop"
  install -m 0644 "$ROOT/packaging/homing.svg"         "$TREE/usr/share/icons/hicolor/scalable/apps/homing.svg"
  install -m 0644 "$ROOT/packaging/org.homing.helper.policy" "$TREE/usr/share/polkit-1/actions/org.homing.helper.policy"
  install -m 0644 "$ROOT/packaging/example-profile.json"   "$TREE/usr/share/homing/examples/profile.example.json"
  install -m 0644 "$ROOT/packaging/copyright"              "$TREE/usr/share/doc/homing-client/copyright"
  install -m 0755 "$ROOT/packaging/postinst"               "$TREE/DEBIAN/postinst"
  install -m 0755 "$ROOT/packaging/prerm"                  "$TREE/DEBIAN/prerm"
  install -m 0755 "$ROOT/packaging/postrm"                 "$TREE/DEBIAN/postrm"

  SIZE=$(du -sk "$TREE" | cut -f1)
  sed -e "s/@VERSION@/$VERSION/" -e "s/@ARCH@/$ARCH/" -e "s/@SIZE@/$SIZE/" \
      "$ROOT/packaging/control.in" > "$TREE/DEBIAN/control"

  DEB="$DIST/homing-client_${VERSION}_${ARCH}.deb"
  # -Zxz：UOS 20 / Debian 10 的 dpkg 1.19 对 zstd 支持有限，用 xz 最稳
  dpkg-deb -Zxz --build --root-owner-group "$TREE" "$DEB" >/dev/null
  echo "已生成: $DEB"
done

echo
ls -lh "$DIST"/*.deb
