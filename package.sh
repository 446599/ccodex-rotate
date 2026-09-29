#!/bin/sh
# Package ccodex-rotate into per-platform zip archives under ./release.
#
# Each archive unpacks to a self-contained folder:
#   pkg-<os>-<arch>/
#     ccodex-rotate[.exe]      main binary
#     mihomo[.exe]             bundled proxy core (found next to the binary)
#     start.command|start.cmd  launcher
#     config.example.json
#     LICENSE.mihomo
#     THIRD_PARTY_NOTICES.txt
#
# Cores are located (first match wins) at:
#   1. $CORES_DIR/<os>-<arch>/mihomo[.exe]   (CORES_DIR defaults to ./cores)
#   2. extracted from release/ccodex-rotate-0.5.0-<os>-<arch>.zip (then cached)
# Run ./build.sh first (or this script will build for you).
set -e
cd "$(dirname "$0")"

VER="${1:-$(sed -n 's/^const version = "\(.*\)"/\1/p' cmd/ccodex-rotate/main.go | head -1)}"
[ -n "$VER" ] || { echo "cannot determine version" >&2; exit 1; }
CORES_DIR="${CORES_DIR:-cores}"

[ -x dist/ccodex-rotate-darwin-arm64 ] || sh build.sh

mkdir -p release

ensure_core() {
  os="$1"; arch="$2"; core="$3"
  dst="$CORES_DIR/$os-$arch/$core"
  if [ -f "$dst" ]; then printf '%s' "$dst"; return 0; fi
  legacy="release/ccodex-rotate-0.5.0-$os-$arch.zip"
  [ -f "$legacy" ] || return 1
  mkdir -p "$CORES_DIR/$os-$arch"
  rm -rf "$CORES_DIR/.tmp"; mkdir -p "$CORES_DIR/.tmp"
  unzip -qo "$legacy" "pkg-$os-$arch/$core" -d "$CORES_DIR/.tmp" >/dev/null 2>&1 || true
  if [ -f "$CORES_DIR/.tmp/pkg-$os-$arch/$core" ]; then
    mv "$CORES_DIR/.tmp/pkg-$os-$arch/$core" "$dst"
    rm -rf "$CORES_DIR/.tmp"
    printf '%s' "$dst"; return 0
  fi
  rm -rf "$CORES_DIR/.tmp"
  return 1
}

for plat in darwin/arm64 darwin/amd64 windows/amd64 windows/arm64; do
  os=${plat%/*}; arch=${plat#*/}
  core="mihomo"; bin="ccodex-rotate"; launcher="start.command"
  if [ "$os" = windows ]; then
    core="mihomo.exe"; bin="ccodex-rotate.exe"; launcher="start.cmd"
  fi
  srcbin="dist/ccodex-rotate-$os-$arch"
  [ "$os" = windows ] && srcbin="$srcbin.exe"
  [ -f "$srcbin" ] || { echo "missing $srcbin (run ./build.sh)" >&2; exit 1; }
  coresrc=$(ensure_core "$os" "$arch" "$core") || {
    echo "no mihomo core for $os/$arch; place it at $CORES_DIR/$os-$arch/$core" >&2
    exit 1
  }

  pkgdir="release/pkg-$os-$arch"
  rm -rf "$pkgdir"; mkdir -p "$pkgdir"
  cp "$srcbin" "$pkgdir/$bin"
  cp "$coresrc" "$pkgdir/$core"
  cp "$launcher" "$pkgdir/$launcher"
  cp config.example.json LICENSE.mihomo THIRD_PARTY_NOTICES.txt "$pkgdir/"
  chmod +x "$pkgdir/$bin" "$pkgdir/$core" "$pkgdir/$launcher" 2>/dev/null || true

  zipname="ccodex-rotate-$VER-$os-$arch.zip"
  rm -f "release/$zipname"
  ( cd release && zip -qr "$zipname" "pkg-$os-$arch" )
  echo "packed release/$zipname"
done

rm -rf release/pkg-*
( cd release && shasum -a 256 ccodex-rotate-"$VER"-*.zip > checksums.txt )
echo "done -> release/ (checksums.txt written)"
