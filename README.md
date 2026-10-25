# WhatsApp para o Claude

[![CI](https://github.com/rodrigopg/whatsapp-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/rodrigopg/whatsapp-mcp/actions/workflows/ci.yml)

**Seu WhatsApp dentro do Claude (ou ChatGPT).** Peça "o que ficou pendente hoje?" ou "responde o João que eu chego às 15h".

## O que faz

- Lê e busca suas conversas, áudios (transcreve) e arquivos
- Envia mensagens, áudios e arquivos, em contatos e grupos
- Mostra o que ficou **pendente**: quem te cobrou e o que você prometeu
- Resumo da manhã: só o que precisa da sua ação hoje
- Cria o conjunto de dados da **sua voz** para clonar (`/whatsapp-mcp:clone`)

## Instalar (Claude Code)

```
/plugin marketplace add rodrigopg/claude-plugins
/plugin install whatsapp-mcp@rodrigopg
/whatsapp-mcp:setup
```

O `setup` guia o resto: instala, mostra um QR code e você escaneia com o celular (Aparelhos conectados). Pronto.

## Seguro?

- Suas mensagens ficam **no seu computador** (banco local). Só vão para a IA quando você pede.
- Uma mensagem maliciosa pode tentar enganar a IA ("prompt injection"). Para acesso remoto, use o modo somente leitura (`MCP_READONLY=true`).
- Projeto independente, código aberto, não é do WhatsApp/Meta. Use por sua conta e risco.

Ajuda: abra uma [issue](https://github.com/rodrigopg/whatsapp-mcp/issues). Segurança: [SECURITY.md](SECURITY.md).

---

<details>
<summary><b>Parte técnica (nerd / para I.A. ler) — English, documentação completa</b></summary>

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
- **REST API bound to `127.0.0.1` by default** — the upstream bound to `0.0.0.0`, meaning anyone on the same LAN could send messages as you. Set `BIND_ADDR=<ip>` to opt into wider exposure (`0.0.0.0`, or a specific interface like a Tailscale address). If `BIND_ADDR` is not loopback, `API_AUTH_TOKEN` becomes mandatory — the bridge refuses to start without it, and every `/api/*` request must send `Authorization: Bearer <token>`. `GET /healthz` (connected yes/no, nothing else) and `/qr` stay open without a token.

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

Optional, for both engines:
- `TRANSCRIPTION_LANGUAGE` — ISO-639-1 code of your voice notes (`pt`, `en`, `es`, ...). Defaults to `pt` (Portuguese), preserving existing installs. Set `auto` explicitly to let Whisper detect the language per audio.
- `TRANSCRIPTION_PROMPT` — biases transcription toward correct punctuation and domain terms. For `pt`, the default is `A seguir, a transcrição de um áudio. A transcrição deve ser precisa, com pontuação e capitalização corretas.` Other languages and `auto` have no default prompt. Set this variable to override the prompt for any language; an empty value (`TRANSCRIPTION_PROMPT=""`) disables it. Write prompts in the language of the audios.

Voice notes **you sent** are skipped by default (an API engine would otherwise upload audio you never asked to transcribe). Set `TRANSCRIBE_OUTGOING=1` to include them, e.g. for `/whats:clone`, which reads transcripts of your own audio.

**Where these vars must live** depends on how the bridge starts — the 5-minute sweep inherits the bridge process's environment, *not* your interactive shell's:
- **`go run main.go` / running `./whatsapp-bridge` by hand** — `export` them in the same shell first, or prefix the command.
- **`start-bridge.sh` (from `install.sh`) or macOS launchd auto-start** — `export` in your shell will **not** reach launchd. Instead create `~/.whatsapp-mcp/transcription.env` with the vars (one `export VAR=value` per line); `start-bridge.sh` sources it, so both the manual launcher and launchd pick them up.

Once an engine is configured: new voice messages become searchable within ~5 minutes, and **`list_messages` matches their transcribed text** like any other message.

When installed as a Claude Code plugin, run the backfill/recovery scripts from `~/.whatsapp-mcp` (the runtime install), not from the plugin directory.

The backfill/recovery scripts are separate processes that read the engine vars from *their own* shell — so `source ~/.whatsapp-mcp/transcription.env` (or re-`export` the vars) in the shell you run them from; the `transcription.env` you set up for the bridge does not reach them automatically. Then, from `whatsapp-mcp-server/`:
- **`python3 transcribe.py`** — backfill existing audios that are still downloadable.
- **`python3 recover_audios.py`** — for audios that expired from WhatsApp's CDN (shown as `[áudio indisponível…]`); requires your **phone online**. This one scrapes the bridge's log to confirm each re-upload, so the bridge must be logging to a *file* and `WHATSAPP_BRIDGE_LOG` must point at it (a foreground `go run main.go` logs to the terminal, not a file). With the `install.sh` launchd setup the log is at `~/.whatsapp-mcp/bridge.log`, so run `WHATSAPP_BRIDGE_LOG=~/.whatsapp-mcp/bridge.log python3 recover_audios.py`.

### Open matters, favorites and the morning summary

For people who lose track of what they promised and who is waiting for an answer. Three Claude Code commands built on the tools below:

- `/whats:triage` reviews the new messages and keeps a list of **open matters**: requests and chasers addressed to you, promises (yours or someone else's) and agreed events. A matter is closed only by a *later message from the other person* confirming it, or by you: silence, your own reply and a courtesy "ok"/"vou ver" never close anything. Matters expire one month after they arose; a closed one reopens if the conversation returns to it. In groups only messages that @-mention you count.
- `/whats:favorites` suggests your favorites from last month's two-way conversations (at least 3 messages each way, one-to-one chats only) and records only what you accept; it also mirrors the WhatsApp Favorites list when WhatsApp syncs it.
- `/whats:morning` runs the review and shows **one summary a day**: only what needs your action, favorites first, then who chased you or what you promised, then the rest (10 items plus a "more N" count; the summary is stored as a photograph that does not change that day). It also writes **chase drafts** for matters other people owe you: nothing is ever sent until you approve a specific draft, and then exactly the text you approved goes out, once. It explains how to run itself every morning (`/schedule` or `/loop`) but never creates the schedule itself.

How it stays private: the AI client you already use (Claude Code) is the one that reads the messages and recognizes requests and promises; the bridge only stores the state and applies fixed rules (one-month validity, when something needs attention, never closing by silence). No extra service is called and nothing is sent without your approval. Write tools are hidden under `MCP_READONLY`.

Also new: `list_active_chats` ("who did I talk to on that day?", one row per conversation with a snippet and a link) and `send_contact` (a native contact card).

### Voice dataset

`/whats:clone` builds a voice-cloning / TTS fine-tuning dataset from **your own** sent voice notes (24 kHz mono wavs, transcripts from the bridge's transcription sweep, a `train_raw.jsonl` and an LJSpeech-style `metadata.csv`). Only `is_from_me` audio is read, everything stays local. See `commands/clone.md`.

### Voice messages from text (opt-in)
`send_voice_message(recipient, text, notice="")` turns text into speech and sends it as a WhatsApp voice message through the same path as `send_audio_message`. It is off until you configure an engine: with `TTS_ENGINE` unset, the tool returns a "TTS not configured" error and nothing is generated or sent. Text over 4096 characters (`MAX_TTS_CHARS`) is refused, never truncated. Either engine needs `ffmpeg` (the audio is converted to Opus Ogg and the temporary file is deleted after sending). Set these in the environment of the MCP server process. `TTS_TIMEOUT` (seconds, default 120) bounds one synthesis; raise it for a slow CPU-only local voice.

The optional `notice` is a text message sent right before the voice note, and only after the speech was generated, so the two arrive together instead of the text waiting on synthesis. If generation fails nothing is sent; if the notice cannot be sent, the voice note is not sent and the reason is returned.

- **Local** — no API cost, nothing leaves your machine. Point `TTS_CLI` at a program that reads the text on stdin and writes a WAV to `--output_file` (the shape of the [piper](https://github.com/OHF-Voice/piper1-gpl) CLI), and `TTS_MODEL` at the voice model:
  ```sh
  export TTS_ENGINE=local
  export TTS_CLI=/path/to/piper
  export TTS_MODEL=/path/to/pt_BR-cadu-medium.onnx
  ```
- **API (any OpenAI-compatible `/audio/speech` endpoint)** — audio is generated remotely. **With `TTS_ENGINE=api` the text of every voice message leaves your machine and is sent to a third-party service (OpenAI by default, or whatever `TTS_API_BASE` points at).** Use `local` if the text must not leave the machine. Error messages returned to MCP clients carry only an HTTP status or exit code; details go to the server log.
  ```sh
  export TTS_ENGINE=api
  export TTS_API_KEY=sk-...
  # optional, defaults shown:
  #   TTS_API_BASE=https://api.openai.com/v1
  #   TTS_API_MODEL=gpt-4o-mini-tts
  #   TTS_API_VOICE=alloy
  ```

The voice model is never part of this repo. For Brazilian Portuguese, [`rhasspy/piper-voices`](https://huggingface.co/rhasspy/piper-voices) has `pt/pt_BR` voices (cadu, used in the example above, plus faber, jeff and edresson). They are fine-tunes of the `lessac` (en_US) voice, whose dataset carries its own license (Blizzard 2013), so check the terms before using one for anything beyond personal use.

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

**Read-only and remote MCP (opt-in).** Both are off by default; stdio installs behave exactly as before.

- `MCP_READONLY=true` — registers only the read tools (`search_contacts`, `list_messages`, `list_chats`, `list_active_chats`, `list_matters`, `review_next_messages`, `list_chase_drafts`, `suggest_favorites`, `list_favorites`, `get_chat`, `get_direct_chat_by_contact`, `get_contact_chats`, `get_last_interaction`, `get_message_context`, `get_group_info`, `resolve_contact`, `check_whatsapp`, `get_poll_votes`). Tools that can change state under any argument are treated as writes, so `get_group_invite_link` (its `reset` flag revokes the link) is hidden too. Every write tool, including `download_media` (it writes files to disk), is not exposed at all. Works with either transport.
- `MCP_TRANSPORT=streamable-http` — serves MCP over HTTP instead of stdio, at `http://<MCP_HOST>:<MCP_PORT>/mcp` (defaults `127.0.0.1` and `8000`; `/mcp/` works too). `MCP_AUTH_TOKEN` is **mandatory**: the server refuses to start without it, and every request needs `Authorization: Bearer <token>` (checked in constant time, 401 otherwise).

```bash
MCP_TRANSPORT=streamable-http MCP_READONLY=true MCP_AUTH_TOKEN="$(openssl rand -hex 32)" \
  uv run --directory whatsapp-mcp-server main.py
```

**Claude web (claude.ai) custom connector (opt-in).** Claude web reaches your server from Anthropic's cloud, so it needs a public `https://` URL and OAuth (some plans can instead send a fixed `Authorization: Bearer <MCP_AUTH_TOKEN>` header: choose "No sign-in" and add it under "Request headers"). Set `MCP_PUBLIC_URL` (the public address, no `/mcp`) and `MCP_OAUTH_PASSPHRASE` (24+ characters, `openssl rand -hex 24`) next to `MCP_AUTH_TOKEN` and the server also becomes a tiny single-owner OAuth 2.1 server: discovery, dynamic client registration (only Claude's callbacks and loopback ones are accepted), authorization code with PKCE, one-hour access tokens, rotating 30-day refresh tokens (stored hashed in `MCP_OAUTH_DB`, default `mcp-oauth.db`). You approve a connection by typing the passphrase on the login page (10 wrong tries per 15 minutes blocks everyone for a while). Then add `https://<your host>/mcp` under Customize > Connectors > Add custom connector. **ChatGPT** works the same way (Settings > Connectors > Advanced > Developer mode, then create a connector with the URL and OAuth; Plus, Pro, Business, Enterprise and Edu plans): its callbacks (`chatgpt.com`, `chat.openai.com`) are accepted at registration. Read tools carry `readOnlyHint`, so both clients can treat them as reads. Put TLS in front (reverse proxy); the app itself speaks plain HTTP. Anyone with the passphrase or a valid token can read your messages, and send them unless `MCP_READONLY=true`, so keep both secret and consider `MCP_READONLY=true` for work accounts. Claude asks you to approve each tool call unless you choose "Always allow".

Security: the HTTP transport has no TLS. A bearer token over plain HTTP is readable by anyone on the path, so keep the default loopback bind and reach it through an SSH tunnel, Tailscale/WireGuard, or a TLS-terminating reverse proxy. Prefer `MCP_READONLY=true` for any remote client: a leaked token on a read-only server cannot send, delete or download anything, but it can still read all your messages. Requires `mcp>=1.9` (the lockfile pins 1.9.4).

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

The image has a `HEALTHCHECK` (and compose a matching `healthcheck:`) that polls `GET /healthz`: 200 `{"status":"ok","connected":true}` when WhatsApp is connected, 503 `{"status":"degraded","connected":false}` otherwise. Check with `docker compose ps`.

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

- Go 1.25+ (go.mod asks for Go 1.26 and the patched 1.27.3 toolchain: Go downloads it by itself the first time)
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

**Edits and deletions:** incoming message edits update the stored text in place (an edit never blanks a message nor overwrites an audio transcription), and incoming "delete for everyone" keeps the row but replaces its content with `[message deleted by the sender]`.

---

## MCP Tools

| Tool | Description |
|------|-------------|
| `search_contacts` | Search contacts by name or phone number (LID-aware) |
| `list_messages` | Retrieve messages with filters, pagination, context |
| `list_chats` | List chats with metadata |
| `suggest_favorites` | Suggest favorites from last month's two-way conversations; only proposes |
| `list_favorites` | The favorites: WhatsApp's Favorites list plus the suggestions you accepted |
| `decide_favorite` | Record your decision for one contact: accept, reject or remove |
| `morning_summary` | The day's summary: only what needs your action, favorites first, max 10 plus a "more N" count; one photograph per day |
| `create_chase_draft` | Write a draft chasing someone about a matter they owe; sends nothing |
| `list_chase_drafts` | List chase drafts (pending, sent, discarded) |
| `approve_chase_draft` | Send a draft in your name: only the exact text you approved goes out |
| `discard_chase_draft` | Discard a pending draft; it is never sent later |
| `list_matters` | Open matters (pending requests and commitments) with state, proof and why they need attention on a day |
| `review_next_messages` | Messages not reviewed yet, with the open matters of those chats; never moves the cursor |
| `record_matter` | Record a matter citing the message that proves it (or add a mention to an existing one) |
| `close_matter` | Close a matter: confirmed by a later message from the other person, or by the owner |
| `reopen_matter` | Reopen a closed or expired matter when the conversation returns to it |
| `mark_reviewed` | Advance the review cursor |
| `send_contact` | Send a contact as a native contact card (vCard) |
| `vote_in_poll` | Cast (or retract) your own vote in a poll |
| `list_active_chats` | Who you talked to in a period (e.g. one day): one row per conversation with first/last time, count, last-message snippet and a link |
| `get_chat` | Get info about a specific chat |
| `get_direct_chat_by_contact` | Find a direct chat by phone number (LID-aware) |
| `get_contact_chats` | All chats involving a contact |
| `get_last_interaction` | Most recent message with a contact |
| `get_message_context` | Messages around a specific message |
| `send_message` | Send a text message, optionally as a native reply quoting a stored text message and/or @-mentioning people |
| `send_file` | Send image, video, document, or audio file |
| `send_audio_message` | Send audio as a WhatsApp voice message |
| `send_voice_message` | Convert text to speech and send it as a voice message, with an optional `notice` text sent just before it (opt-in, see [Voice messages from text](#voice-messages-from-text-opt-in)) |
| `download_media` | Download media from a message, get local path |
| `create_group` | Create a new WhatsApp group |
| `leave_group` | Leave a group |
| `update_group_participants` | Add/remove/promote/demote group members |
| `send_chat_presence` | Send typing or recording indicators |
| `get_group_invite_link` | Get a group invite link, or revoke it and get a new one (`reset`) |
| `join_group_with_link` | Join a group by invite link or code |
| `update_group_settings` | Set group name, description, announce-only, locked |
| `check_whatsapp` | Check if phone numbers are registered on WhatsApp |
| `create_poll` | Send a poll (2-12 options) to a chat |
| `get_poll_votes` | Read the votes of a poll the bridge saw live |

---

## Troubleshooting

- **QR code not displaying**: terminal QR not working? Check `/tmp/whatsapp-qr.png` (macOS opens it automatically).
- **Contacts showing as numbers**: the bridge syncs names on connect. Give it a few seconds after the "Connected" message.
- **LID contacts not found**: happens when WhatsApp hasn't yet synced the LID→PN mapping locally. Reconnect to trigger a fresh sync.
- **Bridge lost the session**: on logout (unlinked from the phone, or the session expired) the bridge logs the reason and exits with code 1; under the launchd/systemd service it restarts and serves a fresh QR at `/qr`. When a second bridge connects with the same `store/` WhatsApp drops the first one ("Another client took over this WhatsApp session"): the bridge backs off from 30 s up to 30 min before reconnecting, so stop the duplicate. A temporary ban or an outdated client is logged with the reason; reads keep working from the local database meanwhile. A paired bridge that stays offline for any other reason is reconnected by a watchdog every minute.
- **Out of sync / re-pairing**: deleting `whatsapp-bridge/store/whatsapp.db` (or re-scanning the QR for any reason) forces WhatsApp to re-deliver up to a year of history. The re-sync keeps existing transcriptions (an incoming empty `content` never overwrites a stored one), but it is still a bulk rewrite of `messages.db`: **back up `whatsapp-bridge/store/messages.db` and `whatsapp.db` first**. Deleting `messages.db` loses every transcription, since the sync brings audio back without text; run `transcribe.py` / `recover_audios.py` afterwards to rebuild them.
- **Device limit**: WhatsApp limits linked devices. Remove one via Settings → Linked Devices on your phone.
- **Dev clone or custom port**: point the MCP server at your setup via the `WHATSAPP_BRIDGE_PORT` or `WHATSAPP_API_BASE_URL` env vars.

---

## Testing

CI runs the Go build/vet/tests and the Python unit tests on every PR. Changes that touch WhatsApp behavior are also validated live against two paired accounts; see [tests/e2e/README.md](tests/e2e/README.md). Security reports: see [SECURITY.md](SECURITY.md).

---

## Credits

- Originally based on [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp) by Luke Harries (MIT)
- WhatsApp web protocol library: [whatsmeow](https://github.com/tulir/whatsmeow)
- PRs cherry-picked from: #209 (coucaj), #221 (fpto), #239 (jayeshkaithwas), #244 (realitix)

</details>
