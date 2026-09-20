#!/bin/sh
# Build ccodex-rotate for macOS and Windows into ./dist
set -e
cd "$(dirname "$0")"
mkdir -p dist
for t in darwin/arm64 darwin/amd64 windows/amd64 windows/arm64; do
  os=${t%/*}
  arch=${t#*/}
  out="dist/ccodex-rotate-$os-$arch"
  [ "$os" = windows ] && out="$out.exe"
  echo "building $out"
  GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$out" ./cmd/ccodex-rotate
done
echo "done -> dist/"
