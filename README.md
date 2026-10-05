# WhatsApp MCP Server

> Independent, maintained project originally based on [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp).

This is a Model Context Protocol (MCP) server for WhatsApp.

With this you can search and read your personal WhatsApp messages (including images, videos, documents, and audio messages), search your contacts and send messages to either individuals or groups. You can also create and leave groups.

It connects to your **personal WhatsApp account** directly via the WhatsApp web multidevice API (using the [whatsmeow](https://github.com/tulir/whatsmeow) library). All your messages are stored locally in a SQLite database and only sent to an LLM (such as Claude) when the agent accesses them through tools (which you control).

> *Caution:* as with many MCP servers, the WhatsApp MCP is subject to [the lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/). Prompt injection could lead to private data exfiltration.

---

## Highlights

### Bug fixes & compatibility
- **whatsmeow API updated** — the upstream bridge broke when whatsmeow added `context.Context` to all API calls. Fixed across all call sites.
- **PNG QR fallback** — QR code is saved to `/tmp/whatsapp-qr.png` and opened automatically on macOS if the terminal rendering is hard to scan.

### Security
- **REST API bound to `127.0.0.1` by default** — the upstream bound to `0.0.0.0`, meaning anyone on the same LAN could send messages as you. Set `BIND_ADDR=<ip>` to opt into wider exposure (`0.0.0.0`, or a specific interface like a Tailscale address). If `BIND_ADDR` is not loopback, `API_AUTH_TOKEN` becomes mandatory — the bridge refuses to start without it, and every `/api/*` request must send `Authorization: Bearer <token>`.

### Contact name resolution (LID migration)
WhatsApp has been migrating contacts from phone-based JIDs (`+55...@s.whatsapp.net`) to internal LID JIDs (`xxx@lid`) for privacy. This broke contact search, `get_direct_chat_by_contact`, and `list_messages` in the upstream. Fixed with:
- `resolvePhoneToJIDs` (Go bridge) — looks up all JID variants (PN + LID) for a phone number via `whatsapp.db`, exposed to the MCP server through `POST /api/chat/by_contact`
- `POST /api/contacts/search` searches `whatsmeow_contacts` in `whatsapp.db` first (real names + LID contacts), falling back to `messages.db`
- `get_direct_chat_by_contact` resolves LID JIDs to phone numbers correctly (bridge-side, via the endpoint above)
- `resolveToPN` in the Go bridge normalizes LID→PN at write time so the same contact never splits across two `chat_jid` values
- `migrateLIDChats` runs at startup and merges any existing `@lid` chat rows into their `@s.whatsapp.net` equivalents (transactional, idempotent)

### Contact name display
- New `senders` table in `messages.db` stores `full_name`, `push_name`, `business_name` per sender
- `SyncAllContacts` bulk-upserts the whatsmeow contact store into `senders` on connect and after each history sync
- Incoming messages enrich `senders` with `PushName` + contact store data
- Terminal log now shows human-readable names instead of raw phone numbers
- `GetChatName` checks the local `senders` table before hitting the live contact store

### Outbound message persistence
- Sent text messages are stored locally immediately so your own sends appear in the conversation history without waiting for a multi-device echo (which doesn't fire on single-device accounts)

### Media improvements
- Document uploads now use stdlib MIME detection instead of hardcoding `application/octet-stream`
- `FileName` field set on `DocumentMessage` so files display correctly in WhatsApp

### Group management (new MCP tools)
- **`create_group`** — create a new WhatsApp group with a name and participants; optionally create as a community parent or sub-group
- **`leave_group`** — leave a group by JID

### Audio transcription (opt-in)
Voice messages are stored with empty searchable text by default. Enable transcription to turn them into searchable `content`, written back into the messages DB so the normal (accent-insensitive) search finds them. A 5-minute sweep in the bridge transcribes new audios automatically; `transcribe.py` backfills existing ones, and `recover_audios.py` recovers media that expired from WhatsApp's CDN (via media retry — needs your phone online).

**Transcription is off until you configure an engine.** With nothing set, the sweep is a clean no-op — it never marks audios, so enabling later still picks them all up. Pick one engine:

- **Local (whisper.cpp)** — private, no per-call cost, needs a compiled [whisper.cpp](https://github.com/ggml-org/whisper.cpp) build + a model + `ffmpeg`:
  ```sh
  export TRANSCRIPTION_ENGINE=local
  export WHISPER_CLI=/path/to/whisper.cpp/build/bin/whisper-cli
  export WHISPER_MODEL=/path/to/models/ggml-medium.bin
  ```
- **API (OpenAI Whisper, or any OpenAI-compatible endpoint like Groq)** — zero local setup, audio leaves your machine:
  ```sh
  export TRANSCRIPTION_ENGINE=api
  export TRANSCRIPTION_API_KEY=sk-...           # OpenAI
  # Groq instead:
  #   TRANSCRIPTION_API_KEY=gsk_...
  #   TRANSCRIPTION_API_BASE=https://api.groq.com/openai/v1
  #   TRANSCRIPTION_API_MODEL=whisper-large-v3
  ```

Optional: `TRANSCRIPTION_PROMPT` biases both engines toward correct punctuation and domain terms.

**Where these vars must live** depends on how the bridge starts — the 5-minute sweep inherits the bridge process's environment, *not* your interactive shell's:
- **`go run main.go` / running `./whatsapp-bridge` by hand** — `export` them in the same shell first, or prefix the command.
- **`start-bridge.sh` (from `install.sh`) or macOS launchd auto-start** — `export` in your shell will **not** reach launchd. Instead create `~/.whatsapp-mcp/transcription.env` with the vars (one `export VAR=value` per line); `start-bridge.sh` sources it, so both the manual launcher and launchd pick them up.

Once an engine is configured: new voice messages become searchable within ~5 minutes, and **`list_messages` matches their transcribed text** like any other message.

When installed as a Claude Code plugin, run the backfill/recovery scripts from `~/.whatsapp-mcp` (the runtime install), not from the plugin directory.

The backfill/recovery scripts are separate processes that read the engine vars from *their own* shell — so `source ~/.whatsapp-mcp/transcription.env` (or re-`export` the vars) in the shell you run them from; the `transcription.env` you set up for the bridge does not reach them automatically. Then, from `whatsapp-mcp-server/`:
- **`python3 transcribe.py`** — backfill existing audios that are still downloadable.
- **`python3 recover_audios.py`** — for audios that expired from WhatsApp's CDN (shown as `[áudio indisponível…]`); requires your **phone online**. This one scrapes the bridge's log to confirm each re-upload, so the bridge must be logging to a *file* and `WHATSAPP_BRIDGE_LOG` must point at it (a foreground `go run main.go` logs to the terminal, not a file). With the `install.sh` launchd setup the log is at `~/.whatsapp-mcp/bridge.log`, so run `WHATSAPP_BRIDGE_LOG=~/.whatsapp-mcp/bridge.log python3 recover_audios.py`.

### Configuration
- `WHATSAPP_BRIDGE_PORT` env var — change the REST API port (default `8080`)
- `WHATSAPP_API_BASE_URL` env var — point the Python MCP server at a non-default bridge URL
- `WHATSAPP_API_AUTH_TOKEN` env var — bearer token the MCP server sends as `Authorization: Bearer <token>`; required if the bridge's `API_AUTH_TOKEN` is set
- `BIND_ADDR` env var — change the bind address of the REST API (see [Security](#security) above for the auth requirement this triggers)
- `API_AUTH_TOKEN` env var (bridge) — bearer token required on all `/api/*` requests once `BIND_ADDR` is non-loopback
- Transcription env vars — see [Audio transcription](#audio-transcription-opt-in) above
- `WEBHOOK_URL` / `WEBHOOK_SECRET` env vars — opt-in outbound webhooks, see [Webhooks](#webhooks-opt-in) below

### Webhooks (opt-in)

Off by default. Set `WEBHOOK_URL` (http/https) and `WEBHOOK_SECRET` on the bridge to get a
`POST` for every **live** message (not history sync), so tools like n8n can react without polling.
If `WEBHOOK_URL` is set without `WEBHOOK_SECRET`, webhooks stay disabled and the bridge logs why.

Events: `message.received` and `message.sent`. Body:

```json
{"event":"message.received","timestamp":"2026-01-01T12:00:00Z","chat_jid":"5511999999999@s.whatsapp.net",
 "message_id":"3EB0...","sender":"5511999999999","is_from_me":false,"content":"hello","media_type":""}
```

Headers: `X-Signature: sha256=<hex HMAC-SHA256 of the raw body, keyed with WEBHOOK_SECRET>`,
`X-Event`, `X-Delivery-Id`. Delivery is async (bounded queue of 100, dropped and logged when
full), 5s timeout per attempt, up to 3 attempts (any non-2xx is retried), so a receiver may
see duplicates: dedupe on `X-Delivery-Id` or `message_id`. Secret and content are never logged.

Verify the signature on the raw bytes before parsing (Python/Flask; in n8n use a Webhook node
with "Raw Body" on, then a Code node doing the same HMAC compare):

```python
import hmac, hashlib, os
from flask import Flask, request, abort
app = Flask(__name__)

@app.post("/wa")
def wa():
    mac = hmac.new(os.environ["WEBHOOK_SECRET"].encode(), request.get_data(), hashlib.sha256)
    if not hmac.compare_digest("sha256=" + mac.hexdigest(), request.headers.get("X-Signature", "")):
        abort(401)
    print(request.get_json()["content"])
    return "", 204
```

**Running the MCP server against a remote bridge** (e.g. the bridge lives on a VPS/home
server, Claude Code runs on your laptop): the MCP server talks to the bridge over HTTP only —
it doesn't read any local SQLite file — so pointing `WHATSAPP_API_BASE_URL` at a remote host is
enough. Don't bind the bridge's REST API to a public IP without `API_AUTH_TOKEN` set; a private
network (Tailscale, WireGuard, SSH tunnel) plus the token is the recommended setup.

---

## Installation

### Install as a Claude Code plugin

The fastest path for Claude Code users:

```
/plugin marketplace add rodrigopg/claude-plugins
/plugin install whatsapp-mcp@rodrigopg
```

Then run `/whatsapp-mcp:setup` for guided onboarding: it checks dependencies (Go 1.25+, uv, git), builds and installs the bridge as a service, and walks you through QR pairing. The plugin installs from this repo's `main` branch. The first tool call after install/update may be slow while `uv` resolves dependencies.

---

### One-line install (macOS / Linux / WSL)

```bash
curl -fsSL https://raw.githubusercontent.com/rodrigopg/whatsapp-mcp/main/install.sh | bash
```

The script:
- checks Go 1.25+, Python 3.9+, uv (installs uv if missing)
- clones the repo to `~/.whatsapp-mcp`
- compiles the Go bridge
- writes `claude_desktop_config.json` / `~/.cursor/mcp.json` automatically
- creates a `start-bridge.sh` launcher
- on macOS: writes a launchd plist for optional auto-start

After install, run `~/.whatsapp-mcp/start-bridge.sh`, open **http://localhost:8080/qr** in your browser, scan the QR, then restart Claude Desktop or Cursor.

Optional flags (`bash -s -- --service --codex`):
- `--service` — installs the bridge as a systemd **user** service on Linux (`whatsapp-bridge`, auto-restart, survives logout via linger) or loads the launchd plist with KeepAlive on macOS. If a bridge is already running, service setup is skipped.
- `--codex` — registers the MCP server with the Codex CLI (see below).

---

### Docker (bridge only)

Runs the Go bridge (plus ffmpeg and the optional transcription sweep) in a container. The MCP server stays on the host, as before.

```bash
cp .env.example .env            # set API_AUTH_TOKEN (openssl rand -hex 32)
docker compose up -d --build
docker compose logs -f bridge   # scan the QR, or open http://127.0.0.1:8080/qr
```

Messages and sessions live in `./data` (back it up before re-pairing). The API is published on `127.0.0.1:8080` only. Point the MCP server at it with `WHATSAPP_API_BASE_URL=http://127.0.0.1:8080/api` and `WHATSAPP_API_AUTH_TOKEN=<same token>`.

### Codex CLI

```bash
curl -fsSL https://raw.githubusercontent.com/rodrigopg/whatsapp-mcp/main/install.sh | bash -s -- --codex
```

This writes `[mcp_servers.whatsapp]` into `~/.codex/config.toml`. The write is append-safe: if the key already exists, the file is left untouched and the snippet is printed for manual merge. To configure manually:

```toml
[mcp_servers.whatsapp]
command = "uv"
args = ["--directory", "/path/to/whatsapp-mcp/whatsapp-mcp-server", "run", "main.py"]
env = { WHATSAPP_BRIDGE_PORT = "8080" }
```

---

### Manual install

#### Prerequisites

- Go 1.25+
- Python 3.9+
- Claude Desktop (or Cursor)
- UV: `curl -LsSf https://astral.sh/uv/install.sh | sh`
- FFmpeg — *optional* for sending/converting audio; *required* only if you enable **local** audio transcription (the `local` engine shells out to `ffmpeg`). Not needed for the API engine or core MCP use.

#### Steps

1. **Clone this repository**

   ```bash
   git clone https://github.com/rodrigopg/whatsapp-mcp.git
   cd whatsapp-mcp
   ```

2. **Run the WhatsApp bridge**

   ```bash
   cd whatsapp-bridge
   go run main.go
   ```

   On first run, open **http://localhost:8080/qr** in your browser and scan the QR code with WhatsApp (Settings → Linked Devices → Link a Device). The page auto-refreshes when a new code is generated. On macOS the QR is also saved to `/tmp/whatsapp-qr.png` and opened in Preview.

3. **Connect to the MCP server**

   ```json
   {
     "mcpServers": {
       "whatsapp": {
         "command": "{{PATH_TO_UV}}",
         "args": [
           "--directory",
           "{{PATH_TO_SRC}}/whatsapp-mcp/whatsapp-mcp-server",
           "run",
           "main.py"
         ]
       }
     }
   }
   ```

   - **Claude Desktop**: save as `~/Library/Application Support/Claude/claude_desktop_config.json`
   - **Cursor**: save as `~/.cursor/mcp.json`

4. **Restart Claude Desktop / Cursor**

### Windows

`go-sqlite3` requires CGO. Install [MSYS2](https://www.msys2.org/), add `ucrt64\bin` to `PATH`, then:

```bash
cd whatsapp-bridge
go env -w CGO_ENABLED=1
go run main.go
```

---

## Architecture

```
Claude / Cursor
      ↕ MCP (stdio)
Python MCP Server  (whatsapp-mcp-server/)
      ↕ HTTP REST
Go WhatsApp Bridge (whatsapp-bridge/)
      ↕ WhatsApp Web protocol
   WhatsApp servers
```

**Storage** (`whatsapp-bridge/store/`):
- `messages.db` — chats, messages, senders (local SQLite, written by the bridge)
- `whatsapp.db` — whatsmeow session + contact store (written by whatsmeow)

---

## MCP Tools

| Tool | Description |
|------|-------------|
| `search_contacts` | Search contacts by name or phone number (LID-aware) |
| `list_messages` | Retrieve messages with filters, pagination, context |
| `list_chats` | List chats with metadata |
| `get_chat` | Get info about a specific chat |
| `get_direct_chat_by_contact` | Find a direct chat by phone number (LID-aware) |
| `get_contact_chats` | All chats involving a contact |
| `get_last_interaction` | Most recent message with a contact |
| `get_message_context` | Messages around a specific message |
| `send_message` | Send a text message |
| `send_file` | Send image, video, document, or audio file |
| `send_audio_message` | Send audio as a WhatsApp voice message |
| `download_media` | Download media from a message, get local path |
| `create_group` | Create a new WhatsApp group |
| `leave_group` | Leave a group |
| `update_group_participants` | Add/remove/promote/demote group members |
| `send_chat_presence` | Send typing or recording indicators |
| `check_whatsapp` | Check if phone numbers are registered on WhatsApp |

---

## Troubleshooting

- **QR code not displaying**: terminal QR not working? Check `/tmp/whatsapp-qr.png` (macOS opens it automatically).
- **Contacts showing as numbers**: the bridge syncs names on connect. Give it a few seconds after the "Connected" message.
- **LID contacts not found**: happens when WhatsApp hasn't yet synced the LID→PN mapping locally. Reconnect to trigger a fresh sync.
- **Out of sync / re-pairing**: deleting `whatsapp-bridge/store/whatsapp.db` (or re-scanning the QR for any reason) forces WhatsApp to re-deliver up to a year of history. **This destroys your audio transcriptions** — the re-sync re-inserts every audio row with empty `content`, overwriting transcribed text (the writes use `INSERT OR REPLACE`). Before re-pairing, **back up `whatsapp-bridge/store/messages.db`**. Deleting only `messages.db` does *not* protect transcriptions either: the next sync still arrives empty. After re-pairing you must re-run `transcribe.py` / `recover_audios.py` to rebuild them.
- **Device limit**: WhatsApp limits linked devices. Remove one via Settings → Linked Devices on your phone.
- **Dev clone or custom port**: point the MCP server at your setup via the `WHATSAPP_MESSAGES_DB`, `WHATSAPP_BRIDGE_PORT`, or `WHATSAPP_API_BASE_URL` env vars.

---

## Credits

- Originally based on [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp) by Luke Harries (MIT)
- WhatsApp web protocol library: [whatsmeow](https://github.com/tulir/whatsmeow)
- PRs cherry-picked from: #209 (coucaj), #221 (fpto), #239 (jayeshkaithwas), #244 (realitix)
