---
description: Set up the WhatsApp MCP bridge — check dependencies, install as a service, pair via QR and smoke-test
---

Set up the WhatsApp bridge for this plugin. Follow the steps in order; do not skip ahead.

## 0. Detect existing accounts

Read `~/.claude.json` and list any `mcpServers` keys starting with `whatsapp` (e.g. `whatsapp`, `whatsapp-personal`). Also check for existing install dirs: `~/.whatsapp-mcp*`.

- **None found** → this is a first install. Skip ahead to step 1, use defaults (`~/.whatsapp-mcp`, port 8080, server name `whatsapp`).
- **One or more found** → ask the user with AskUserQuestion:
  - Question: "Found existing WhatsApp account(s): `<list the keys/dirs found>`. What do you want to do?"
  - Options: "Add another account" / "Reconfigure/reinstall an existing one" / "Cancel"
  - If **add another account**: ask a follow-up AskUserQuestion for a short label (e.g. "personal", "work"). Normalize it before using it in any path — lowercase, keep only `[a-z0-9-]`, collapse anything else (spaces, slashes, dots) to `-`. This becomes part of a real filesystem path (`~/.whatsapp-mcp-<label>`) and an MCP server key (`whatsapp-<label>`); a raw, unnormalized label (e.g. containing `/` or `..`) could resolve outside the intended directory or produce an invalid server name. Confirm the derived values (install dir, port, server name) with the user in the question itself (put them in the option descriptions) before proceeding.
  - If **reconfigure**: ask which existing account (list them), then run steps 1-6 against that account's existing dir/port instead of picking new ones.
  - If **cancel**: stop here.

Carry the resolved `INSTALL_DIR` / `BRIDGE_PORT` / `SERVER_NAME` from this step through steps 1-6 below — every reference to "the installer" or "the MCP server" means for this specific account.

## 1. Check dependencies

Verify all three before installing anything. If any is missing, STOP — report which one and its install link. Do not partially install.

- `go version` → must be 1.25 or newer. Missing/old: https://go.dev/dl/
- `uv --version` → any version. Missing: https://docs.astral.sh/uv/getting-started/installation/
- `git --version` → any version. Missing: https://git-scm.com/downloads

## 2. Run the installer with service mode

Prefer the plugin's own copy — it always supports the flags. Set `WHATSAPP_MCP_DIR` and `WHATSAPP_BRIDGE_PORT` to the values resolved in step 0. If `${CLAUDE_PLUGIN_ROOT}/install.sh` exists, run:

```bash
WHATSAPP_MCP_DIR="<resolved INSTALL_DIR>" WHATSAPP_BRIDGE_PORT="<resolved BRIDGE_PORT>" \
  bash "${CLAUDE_PLUGIN_ROOT}/install.sh" --service
```

Otherwise download first and verify the script supports `--service` before executing (an older published copy would silently ignore the flag):

```bash
curl -fsSL https://raw.githubusercontent.com/rodrigopg/whatsapp-mcp/main/install.sh -o /tmp/whatsapp-mcp-install.sh
grep -q -- '--service' /tmp/whatsapp-mcp-install.sh
```

- If the grep matches: `WHATSAPP_MCP_DIR="<resolved INSTALL_DIR>" WHATSAPP_BRIDGE_PORT="<resolved BRIDGE_PORT>" bash /tmp/whatsapp-mcp-install.sh --service`
- If it does NOT match: run without `--service` (same env vars), then start the bridge manually with `<INSTALL_DIR>/start-bridge.sh`, and warn the user that automatic service setup is not available until the published installer updates.

Check the installer's output for `differs from generated plist/unit — not touching it` — this means a service already existed (e.g. from a prior install) and `--service` did NOT register a new one for this account, even though the flag was passed. Don't let this pass silently: tell the user explicitly that no service was created, the bridge is not yet running, and it needs to be started manually before continuing (`<INSTALL_DIR>/start-bridge.sh`, same command step 3 uses either way).

If the installer reports this account's bridge port/process is already in use, that account's installation is already running — do not reinstall; continue to step 4.

After the installer succeeds, pre-warm the MCP server's environment so the first tool call doesn't time out on dependency resolution:

```bash
uv --directory "<resolved INSTALL_DIR>/whatsapp-mcp-server" sync
```

## 3. Pair with WhatsApp

Open `http://localhost:<resolved BRIDGE_PORT>/qr` in the user's default browser yourself (`open <url>` on macOS, `xdg-open` on Linux) — don't just print the URL and wait for them to open it. The page there already auto-refreshes every 20s with a fresh QR, so there's no need to save/serve a static PNG separately.

- The bridge listens on loopback only by design. On a headless/remote machine, `open`/`xdg-open` won't reach the user's actual browser — tell them to port-forward instead: `ssh -L <port>:localhost:<port> <host>` and open the URL locally themselves.

Tell the user to scan it with WhatsApp (Settings → Linked Devices → Link a Device). Don't wait for them to say "done" — poll for it yourself: `curl -s http://localhost:<port>/qr` and check whether the response contains `WhatsApp connected` (the page's own success-state text) instead of the QR page. Poll every ~5s, up to the same 3-minute window the bridge itself waits before giving up on the pairing session (past that, tell the user the QR expired and to re-run this step — a fresh page load gets a new one).

## 4. Register the MCP server in Claude Code

`install.sh` writes Claude Desktop/Cursor config automatically, but **not** `~/.claude.json` (Claude Code's own config) — that entry must be added here.

Read `~/.claude.json`, add (or update, if reconfiguring) an entry under `mcpServers` using `<resolved SERVER_NAME>` as the key:

```json
"<SERVER_NAME>": {
  "command": "uv",
  "args": ["--directory", "<resolved INSTALL_DIR>/whatsapp-mcp-server", "run", "main.py"],
  "env": { "WHATSAPP_API_BASE_URL": "http://localhost:<resolved BRIDGE_PORT>/api" }
}
```

Preserve every other existing entry in `mcpServers` — only add/replace this one key. Then tell the user to restart Claude Code (or reconnect MCP servers) so the server picks up the fresh install.

## 5. Smoke test

Call `mcp__<SERVER_NAME>__search_contacts` with a short query. If it returns data (even an empty list without error), report setup as successful.

If it fails:
- Bridge not running? Check `pgrep -fa whatsapp-bridge` and report the error.
- Bridge running but tools still fail? Likely a port mismatch — confirm `WHATSAPP_API_BASE_URL` in this server's `~/.claude.json` env matches the bridge's actual port.
- Multiple accounts and this smoke test returns the wrong data (e.g. same chats as another account)? Do NOT proceed — that means two accounts are pointing at the same bridge port or install dir. Re-check step 0's resolved values before continuing.

## 6. Transcription (optional)

Audio transcription is opt-in — with nothing configured, voice messages stay unsearchable but the bridge works fine. Ask with AskUserQuestion: "Enable audio transcription for this account?" — options "Yes, set it up now" / "Skip for now".

If skipped: stop here, mention `<INSTALL_DIR>/README.md`'s "Audio transcription" section covers enabling it later.

If yes, ask which engine with AskUserQuestion — "API (OpenAI/Groq/compatible) (Recommended)" first (no local build, no multi-GB download, Groq has a generous free tier) / "Local (whisper.cpp)" second:

**Local**: check if `whisper-cli` is already on `PATH` or at a path the user provides. If found (with a model file — ask for its path too, `.bin` under a `models/` dir is the usual layout), write both to `<INSTALL_DIR>/transcription.env`. If NOT found, do not attempt to build whisper.cpp — that's out of scope for this wizard (it's a multi-GB compile from source). Tell the user local requires building https://github.com/ggml-org/whisper.cpp manually first, then ask if they want to switch to the API option instead or skip transcription for now.

```
export TRANSCRIPTION_ENGINE=local
export WHISPER_CLI=<path to whisper-cli>
export WHISPER_MODEL=<path to .bin model>
```

**API**: ask which provider — "Groq (Recommended)" first (generous free tier) / "OpenAI" / "Other OpenAI-compatible endpoint". For Groq or "other", also ask for the base URL and model name (suggest Groq defaults if they pick Groq: `https://api.groq.com/openai/v1`, `whisper-large-v3`).

Then ask "Already have an API key?" with AskUserQuestion:
- **"No, help me create one"**: give the shortest real path to a working key for the chosen provider — Groq: https://console.groq.com/keys (sign up if needed, "Create API Key", copy it) — a couple of terse steps, not a tutorial. Then loop back to asking for the key.
- **"Yes, I'll paste it"**: prompt for it directly.
- Any secrets-manager skill/MCP tool visible in this session (e.g. a password manager) → add a third option offering to look it up there, worded generically ("Look it up in <tool name>") — don't hardcode a specific product name in this file, detect what's actually available in the running session.

Whichever path, never echo the key back in full — confirm receipt by length/prefix only (e.g. "got a key starting with sk-...").

```
export TRANSCRIPTION_ENGINE=api
export TRANSCRIPTION_API_KEY=<key>
# only if Groq/other:
export TRANSCRIPTION_API_BASE=<base url>
export TRANSCRIPTION_API_MODEL=<model>
```

Either engine: ask which language their voice notes are in and set `TRANSCRIPTION_LANGUAGE` to its ISO-639-1 code (`pt`, `en`, ...), or explicitly to `auto` if they mix languages. Leaving it unset keeps Portuguese (`pt`) and the original Portuguese punctuation prompt. Other languages and `auto` have no default prompt. Then ask once, optionally, if they want `TRANSCRIPTION_PROMPT` set (biases transcription toward correct punctuation/domain terms — e.g. product names, jargon they use often). An explicit value overrides the prompt for any language; an empty value (`TRANSCRIPTION_PROMPT=""`) disables it. If a global `CLAUDE.md` or similar user profile is visible in this session and names a specific stack/domain (technologies, tools, jargon they work with), offer that as one of the AskUserQuestion options pre-filled — most users don't have their own jargon list top of mind, but recognize it instantly when it's already drafted from what they told Claude about themselves. Skip if they don't have anything specific in mind, don't push for an answer.

Write the resulting `export VAR=value` lines to `<INSTALL_DIR>/transcription.env` — this file holds a live API key in plaintext, so create it with owner-only permissions from the start (e.g. `(umask 077; cat > "<INSTALL_DIR>/transcription.env" <<'EOF' ... EOF)`), not a default-mode write followed by a separate `chmod`. If the file already existed (reconfiguring), `chmod 600` it regardless of whether the content changed — an earlier install may have left it world-readable. `start-bridge.sh` already sources it if present — no other wiring needed. Tell the user transcription takes effect within ~5 minutes for new voice messages (the bridge's sweep interval).

If there's any existing history synced (chats/messages already present — this is common, pairing pulls recent history), ask with AskUserQuestion whether to backfill transcriptions for existing audio now: "Yes, backfill now" / "Skip — I'll run it later". If yes, run it directly rather than just printing the command — `transcribe.py` does NOT auto-load `transcription.env` (only the bridge's own `start-bridge.sh` sources it), and it needs to know this account's actual bridge port to download audio (its own default is 8080, wrong for any account not on that port):

```bash
cd "<INSTALL_DIR>/whatsapp-mcp-server" && \
  set -a && source "<INSTALL_DIR>/transcription.env" && set +a && \
  WHATSAPP_BRIDGE_PORT="<resolved BRIDGE_PORT>" uv run transcribe.py
```

If this bridge's `BIND_ADDR` is non-loopback (`API_AUTH_TOKEN` required — see the REST API section above), also export `WHATSAPP_API_AUTH_TOKEN=<that same token>` before running the command above, or the download call 401s.

Report how many audios were transcribed when it finishes. If skipped, mention the same command for later.

## 7. Favorites (optional)

Offer to run `/whats:favorites`: it suggests the contacts the user talks to most (both ways, last month) so they can pick their favorites, which then come first in summaries. Only names the user accepts are recorded. Skip silently if the user declines.
