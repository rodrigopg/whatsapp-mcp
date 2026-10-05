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
  # exec makes the subshell BECOME the bridge, so $! is the bridge's own pid (a plain `a && b &` backgrounds
  # the whole list and $! would be the wrapper, which stop would kill while the bridge lived on).
  # The subshell's stdout/stderr are detached too, or a caller capturing our output never sees EOF.
  ( cd ".data/$1" && BIND_ADDR=127.0.0.1 WHATSAPP_BRIDGE_PORT="$(port "$1")" API_AUTH_TOKEN="$E2E_TOKEN" \
      exec "$BIN" > "$HERE/.run/$1.log" 2>&1 < /dev/null ) > /dev/null 2>&1 &
  echo $! > ".run/$1.pid"
}
stop() {
  alive "$1" || { rm -f ".run/$1.pid"; return; }
  local pid; pid=$(cat ".run/$1.pid"); kill "$pid"
  for _ in 1 2 3 4 5 6 7 8 9 10; do kill -0 "$pid" 2>/dev/null || break; sleep 0.5; done
  kill -9 "$pid" 2>/dev/null || true
  rm -f ".run/$1.pid"
}

case "$1" in
  build)   (cd ../../whatsapp-bridge && go build -o "$BIN" .) ;;
  start)   for n in ${2:-a b}; do start "$n"; done ;;
  stop)    for n in ${2:-a b}; do stop "$n"; done ;;
  restart) for n in ${2:-a b}; do stop "$n"; sleep 2; start "$n"; done ;;
esac
