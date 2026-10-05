#!/usr/bin/env bash
# Runs the two e2e bridges as plain processes (no Docker). Usage: native.sh build|start|stop|restart [a|b]
# Sessions live in .data/<a|b>/store (the bridge's cwd is .data/<a|b>).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"
set -a; . ./.env; set +a
BIN="$HERE/.run/whatsapp-bridge"
mkdir -p .run

port() { [ "$1" = a ] && echo "$E2E_A_PORT" || echo "$E2E_B_PORT"; }
alive() { [ -f ".run/$1.pid" ] && kill -0 "$(cat ".run/$1.pid")" 2>/dev/null; }

start() {
  alive "$1" && return
  mkdir -p ".data/$1"
  # The subshell's own stdout/stderr must be detached too, or a caller capturing our output (run.sh) never sees EOF.
  ( cd ".data/$1" && BIND_ADDR=127.0.0.1 WHATSAPP_BRIDGE_PORT="$(port "$1")" API_AUTH_TOKEN="$E2E_TOKEN" \
      setsid "$BIN" > "$HERE/.run/$1.log" 2>&1 < /dev/null & echo $! > "$HERE/.run/$1.pid" ) > /dev/null 2>&1
}
stop() { alive "$1" && kill "$(cat ".run/$1.pid")" || true; rm -f ".run/$1.pid"; }

case "$1" in
  build)   (cd ../../whatsapp-bridge && go build -o "$BIN" .) ;;
  start)   for n in ${2:-a b}; do start "$n"; done ;;
  stop)    for n in ${2:-a b}; do stop "$n"; done ;;
  restart) for n in ${2:-a b}; do stop "$n"; sleep 2; start "$n"; done ;;
esac
