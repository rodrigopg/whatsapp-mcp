# Contributing

Thanks for helping. A short guide so your time is not wasted.

## What this project is
A **personal-history MCP server for WhatsApp**: your own messages, searchable, stored locally, read by an AI agent. Reading, search, transcription and triage are the core. It is not a generic WhatsApp gateway or a bulk-sending bot, and outbound features are kept small. WhatsApp access uses an unofficial client: more automated sending raises ban risk, so features that increase outbound volume need a warning and a good reason.

## What is welcome
- Fixes with a reproduction (a failing test, a log, steps). A test that passes without the fix is not evidence of a bug.
- Robustness of the local history: sync, media, encoding, edge cases of message types.
- New read/search capabilities, additive and opt-in where they change behaviour.
- Parity features that are cheap and safe (groups, polls, edits...) with tests.

## Rules that are not negotiable
- Loopback bind by default; anything else needs a token. No new network surface without auth.
- No secrets, phone numbers or personal paths in the repo.
- Opt-in for anything that sends content to a third party (cloud transcription, TTS, webhooks) with a privacy note in the README. Nothing is sent by default.
- Existing installs must keep working: schema changes are additive (`CREATE ... IF NOT EXISTS`), a re-sync never overwrites stored content, an edit never blanks a row.
- A new MCP tool is registered with `@read_tool` or `@write_tool` (never a bare `@mcp.tool()`); anything that can change state under any argument is a write. A test enforces it.
- Do not return raw bridge error text to MCP clients (paths leak with the remote transport).

## How to send a change
1. Open the PR against `main` (never against another branch: GitHub closes it when that branch is deleted).
2. Keep it small and single-purpose. Put new Go tests in a new `<feature>_test.go` and new Python tests in a new `test_<feature>.py` (they are discovered automatically), so parallel PRs do not conflict. List new MCP tools one name per line in `MCP_TOOLS` in `tests/e2e/e2e.py`.
3. `cd whatsapp-bridge && go build ./... && go vet ./... && go test ./...` and `cd whatsapp-mcp-server && uv run python -m unittest discover -p 'test_*.py'` must pass. CI runs both.
4. The live end-to-end gate (`tests/e2e/`) needs two paired WhatsApp accounts, so it runs on the maintainer's side for maintainer PRs. See `tests/e2e/README.md`.

## What to expect
PRs from contributors are read and reviewed by an automated reviewer and by the maintainer. Contributor code is never executed on the maintainer's hosts by the automation, so the merge is a manual decision by the maintainer. Review comments may be in Portuguese or English.

Security issues: see `SECURITY.md`, please do not open a public issue.
