# WhatsApp MCP Server (Community Fork)

> **Why this fork?** The [upstream repo](https://github.com/lharries/whatsapp-mcp) has been frozen since early 2025 — over 30 open PRs with critical fixes (broken whatsmeow API, LID contact migration, security hardening) were left unmerged. This fork cherry-picks the best of those PRs and keeps the bridge working against current WhatsApp protocol.

This is a Model Context Protocol (MCP) server for WhatsApp.

With this you can search and read your personal WhatsApp messages (including images, videos, documents, and audio messages), search your contacts and send messages to either individuals or groups. You can also create and leave groups.

It connects to your **personal WhatsApp account** directly via the WhatsApp web multidevice API (using the [whatsmeow](https://github.com/tulir/whatsmeow) library). All your messages are stored locally in a SQLite database and only sent to an LLM (such as Claude) when the agent accesses them through tools (which you control).

> *Caution:* as with many MCP servers, the WhatsApp MCP is subject to [the lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/). Prompt injection could lead to private data exfiltration.

---

## What's new in this fork

### Bug fixes & compatibility
- **whatsmeow API updated** — the upstream bridge broke when whatsmeow added `context.Context` to all API calls. Fixed across all call sites.
- **PNG QR fallback** — QR code is saved to `/tmp/whatsapp-qr.png` and opened automatically on macOS if the terminal rendering is hard to scan.

### Security
- **REST API bound to `127.0.0.1` by default** — the upstream bound to `0.0.0.0`, meaning anyone on the same LAN could send messages as you. Set `BIND_ADDR=0.0.0.0` to opt back into LAN exposure.

### Contact name resolution (LID migration)
WhatsApp has been migrating contacts from phone-based JIDs (`+55...@s.whatsapp.net`) to internal LID JIDs (`xxx@lid`) for privacy. This broke contact search, `get_direct_chat_by_contact`, and `list_messages` in the upstream. Fixed with:
- `_resolve_phone_to_jids` — looks up all JID variants (PN + LID) for a phone number via `whatsapp.db`
- `search_contacts` now searches `whatsmeow_contacts` in `whatsapp.db` first (real names + LID contacts), falling back to `messages.db`
- `get_direct_chat_by_contact` resolves LID JIDs to phone numbers correctly
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

The backfill/recovery scripts are separate processes that read the engine vars from *their own* shell — so `source ~/.whatsapp-mcp/transcription.env` (or re-`export` the vars) in the shell you run them from; the `transcription.env` you set up for the bridge does not reach them automatically. Then, from `whatsapp-mcp-server/`:
- **`python3 transcribe.py`** — backfill existing audios that are still downloadable.
- **`python3 recover_audios.py`** — for audios that expired from WhatsApp's CDN (shown as `[áudio indisponível…]`); requires your **phone online**. This one scrapes the bridge's log to confirm each re-upload, so the bridge must be logging to a *file* and `WHATSAPP_BRIDGE_LOG` must point at it (a foreground `go run main.go` logs to the terminal, not a file). With the `install.sh` launchd setup the log is at `~/.whatsapp-mcp/bridge.log`, so run `WHATSAPP_BRIDGE_LOG=~/.whatsapp-mcp/bridge.log python3 recover_audios.py`.

### Configuration
- `WHATSAPP_BRIDGE_PORT` env var — change the REST API port (default `8080`)
- `WHATSAPP_API_BASE_URL` env var — point the Python MCP server at a non-default bridge URL
- `BIND_ADDR` env var — change the bind address of the REST API
- Transcription env vars — see [Audio transcription](#audio-transcription-opt-in) above

---

## Installation

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

---

## Troubleshooting

- **QR code not displaying**: terminal QR not working? Check `/tmp/whatsapp-qr.png` (macOS opens it automatically).
- **Contacts showing as numbers**: the bridge syncs names on connect. Give it a few seconds after the "Connected" message.
- **LID contacts not found**: happens when WhatsApp hasn't yet synced the LID→PN mapping locally. Reconnect to trigger a fresh sync.
- **Out of sync / re-pairing**: deleting `whatsapp-bridge/store/whatsapp.db` (or re-scanning the QR for any reason) forces WhatsApp to re-deliver up to a year of history. **This destroys your audio transcriptions** — the re-sync re-inserts every audio row with empty `content`, overwriting transcribed text (the writes use `INSERT OR REPLACE`). Before re-pairing, **back up `whatsapp-bridge/store/messages.db`**. Deleting only `messages.db` does *not* protect transcriptions either: the next sync still arrives empty. After re-pairing you must re-run `transcribe.py` / `recover_audios.py` to rebuild them.
- **Device limit**: WhatsApp limits linked devices. Remove one via Settings → Linked Devices on your phone.

---

## Credits

- Original project: [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp)
- WhatsApp web protocol library: [whatsmeow](https://github.com/tulir/whatsmeow)
- PRs cherry-picked from: #209 (coucaj), #221 (fpto), #239 (jayeshkaithwas), #244 (realitix)
