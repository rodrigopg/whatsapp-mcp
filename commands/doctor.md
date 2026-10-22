---
description: Diagnose WhatsApp MCP setup — bridge health, auth, pairing, and which account each server points to
---

Diagnose the WhatsApp MCP setup. Read-only — never restart services, rewrite config, or delete files. Report findings as a checklist, not prose.

## 1. Enumerate configured accounts

Read `~/.claude.json`, list every `mcpServers` key starting with `whatsapp`. For each, extract `command`, `args` (the `--directory` value), and `env` (`WHATSAPP_API_BASE_URL`, `WHATSAPP_API_AUTH_TOKEN` if present). The server's base URL is `WHATSAPP_API_BASE_URL` if set, else `http://localhost:8080/api`; use that as `<WHATSAPP_API_BASE_URL>` below.

No `whatsapp*` keys found → report "No WhatsApp MCP server configured — run `/whats:setup`" and stop here.

## 2. Per-account checks

For each configured server, in order:

**a. Install dir exists and has a compiled bridge**
Run b first. If the bridge answers over HTTP, it is built and running wherever it lives (local, remote, or behind an SSH tunnel on localhost); skip this check. Only if it does not answer: check `<--directory>/../whatsapp-bridge/whatsapp-bridge` exists and is executable. Missing → "bridge never built, run install.sh" (for a bridge meant to run on another host, say the host or tunnel is unreachable instead).

**b. Bridge process reachable**
`curl -s -o /dev/null -w "%{http_code}" <WHATSAPP_API_BASE_URL without /api>/qr` (or `/chats` with a POST body `{}` if auth is required — see below). Connection refused/timeout → bridge not running or wrong port. Distinguish these two: `pgrep -fa whatsapp-bridge` tells you if a process exists at all locally; if it does but the port doesn't answer, that's the silent-bind-failure pattern (bridge started before its network interface was ready) — check `ss -tln | grep <port>` and the bridge's own log (`bridge.log` in its install dir) for `bind: cannot assign requested address`.

**c. Auth, if configured**
If `WHATSAPP_API_AUTH_TOKEN` is set in this server's env, POST to `<base>/chats` with `{"limit":1}` and that bearer token. 401 → token doesn't match the bridge's `API_AUTH_TOKEN`. 200 → auth OK, continue.

**d. Session paired**
If (b) succeeded, POST `<base>/chats` `{"limit":1}`. Empty result with no error could mean "genuinely no chats yet" or "never paired" — check the bridge log for `Connected to WhatsApp` vs a QR-waiting state to disambiguate. Never paired → tell the user to open `<base-without-/api>/qr`.

**e. Identity sanity**
If (d) returned a chat, note the chat name/JID in the report. This is the cross-check for the most dangerous failure mode in this project's history: two account configs silently pointing at the same bridge/port, so both MCP servers return identical data. If you're checking 2+ accounts, compare their (d) results — same JID/chat name across two different server names is a real bug, flag it as CRITICAL, not a warning.

## 3. Report

One line per account, per check, pass/fail/skip. End with a one-line verdict per account ("healthy" / "needs pairing" / "wrong token" / "not running") and, only if something failed, the single next command to run (not a menu of options).
