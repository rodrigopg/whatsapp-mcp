#!/usr/bin/env bash
# Validation gate for PRs. Tier 0 is offline; tier 1 needs both bridges paired (QR once, see README.md).
#   ./run.sh          tier 0 + tier 1
#   ./run.sh offline  tier 0 only
set -uo pipefail
cd "$(dirname "$0")"
export PATH="$HOME/.local/bin:$PATH"
ROOT="$(cd ../.. && pwd)"
REPORT="report-$(date +%Y%m%d-%H%M).md"
COMPOSE=(docker compose --env-file .env -f docker-compose.e2e.yml)
FAILED=0

[ -f .env ] || { printf 'E2E_TOKEN=%s\nE2E_A_PORT=8091\nE2E_B_PORT=8092\nE2E_A_PHONE=\nE2E_B_PHONE=\n' "$(openssl rand -hex 32)" > .env; echo "created .env: fill E2E_A_PHONE / E2E_B_PHONE and re-run"; exit 2; }

step() { echo; echo "## $1"; }
run()  { local out; out="$("${@:2}" 2>&1)"; local rc=$?; echo "- $1: $([ $rc -eq 0 ] && echo PASS || echo FAIL)"; [ $rc -eq 0 ] || { echo '```'; echo "$out" | tail -25; echo '```'; FAILED=1; }; }

{
echo "# E2E report $(date '+%Y-%m-%d %H:%M %Z')"
echo "branch: $(git -C "$ROOT" branch --show-current) @ $(git -C "$ROOT" rev-parse --short HEAD)"

step "Tier 0: offline"
run "go build + vet + test" docker run --rm -v "$ROOT/whatsapp-bridge":/src -w /src golang:1.25-bookworm \
    sh -c 'go build -o /tmp/wb . && go vet ./... && go test ./...'
run "python unit tests" sh -c "cd '$ROOT/whatsapp-mcp-server' && uv run python -m unittest test_transcribe test_db_path"
run "compose refuses to start without token" sh -c "! E2E_TOKEN= ${COMPOSE[*]} config"
run "image builds" "${COMPOSE[@]}" build

if [ "${1:-}" != "offline" ]; then
  step "Tier 1: live (A <-> B)"
  run "bridges up" "${COMPOSE[@]}" up -d
  echo '```'
  uv run --project "$ROOT/whatsapp-mcp-server" python e2e.py 2>&1 | tail -80
  RC=${PIPESTATUS[0]}
  echo '```'
  [ "$RC" -eq 0 ] || FAILED=1
fi

step "Result"
echo "$([ $FAILED -eq 0 ] && echo PASS || echo FAIL)"
} | tee "$REPORT"

echo; echo "report: tests/e2e/$REPORT"
exit $FAILED
