#!/bin/sh
# macOS / Linux launcher for ccodex-rotate.
# Double-click or run: ./start.command
DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
CFG="$HOME/.ccodex-rotate/config.json"

BIN="$DIR/ccodex-rotate"
[ -x "$BIN" ] || BIN="$DIR/dist/ccodex-rotate-darwin-arm64"
[ -x "$BIN" ] || BIN="$DIR/dist/ccodex-rotate-darwin-amd64"
if [ ! -x "$BIN" ]; then
  printf '%s\n' "找不到 ccodex-rotate 可执行文件，请先运行 ./build.sh" >&2
  exit 1
fi

if [ ! -f "$CFG" ]; then
  printf '%s\n' "首次运行：请先配置订阅链接，例如"
  printf '  %s sub add "https://你的订阅链接"\n' "$BIN"
  "$BIN" init --config "$CFG"
fi

printf '%s\n' "启动 ccodex-rotate（不修改系统代理，也不影响本地 Clash）。按 Ctrl+C 停止并还原 Codex 配置。"
exec "$BIN" serve --config "$CFG"
