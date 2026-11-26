# Contexto: Servidor MCP Python

Servidor MCP (`whatsapp-mcp-server/`) — expõe tools ao Claude/Cursor via stdio, fala HTTP REST
com a bridge Go pra tudo (leitura E escrita). **Não lê SQLite local** — `whatsapp.py` não tem
`import sqlite3`, é 100% stateless. Isso permite bridge remota (VPS) + MCP local sem
compartilhar filesystem.

## Busca accent-insensitive

- `whatsapp.py`: `_strip_accents(text)` continua existindo (usado por `test_transcribe.py` e
  documentação), mas o `unaccent()` de SQL agora roda **do lado da bridge Go**
  (`sqlite3_unaccent` driver derivado, registrado via `sql.Register` em `main.go`), não mais
  registrado pelo Python.
- `list_messages` e `list_chats` continuam suportando busca accent-insensitive (`query` param),
  só que a lógica de comparação está na query SQL dentro da bridge agora.

## Resolução de contatos (LID-aware)

- Toda resolução LID↔PN roda na bridge Go agora (`resolvePhoneToJIDs`, `getContactNameFromStore`
  em `main.go`) — os antigos `_resolve_phone_to_jids`/`_get_contact_name` em Python foram
  removidos.
- `get_direct_chat_by_contact` chama `POST /api/chat/by_contact`; a bridge resolve LID
  internamente e aplica o fallback de nome (`chat.name` vazio/só-dígitos → busca
  `full_name`/`push_name` em `whatsmeow_contacts`).
- `search_contacts` chama `POST /api/contacts/search`.

## Tools expostas

search_contacts, list_messages, list_chats, list_active_chats, list_matters, review_next_messages, morning_summary, create_chase_draft, list_chase_drafts, approve_chase_draft, discard_chase_draft, suggest_favorites, list_favorites, decide_favorite, record_matter, close_matter, reopen_matter, mark_reviewed, get_chat, get_direct_chat_by_contact, get_contact_chats, get_last_interaction, get_message_context, send_message, send_contact, send_file, send_audio_message, download_media, create_group, leave_group.

## Endpoint da bridge + auth

- `WHATSAPP_API_BASE_URL` aponta o MCP/scripts pra bridge não-default. Default `http://localhost:8080/api`.
- `WHATSAPP_API_AUTH_TOKEN` — obrigatório se a bridge exige auth (ver `bridge-go.md`, seção
  `API_AUTH_TOKEN`). Vai em todo request como `Authorization: Bearer <token>`, inclusive nas
  chamadas de escrita já existentes (`send_message`, `send_file`, etc).
- A bridge passa esse var derivado de `WHATSAPP_BRIDGE_PORT` pro sweep de transcrição, pra download bater na porta certa.

## Endpoints REST consumidos (todos POST, JSON body/response)

`/api/chats`, `/api/messages`, `/api/message_context`, `/api/contacts/search`,
`/api/contacts/chats`, `/api/contacts/last_interaction`, `/api/chat`, `/api/chat/by_contact`,
`/api/sender_name` — implementados em `main.go`, que é o contrato. Timestamps vêm com offset
explícito (ex. `-03:00`), não `Z` — `_parse_ts()` em `whatsapp.py` trata os dois formatos, mas
não depender de `Z` sendo enviado.

## Config no cliente

- Claude Desktop: `~/Library/Application Support/Claude/claude_desktop_config.json`
- Cursor: `~/.cursor/mcp.json`
- Roda via `uv --directory <path>/whatsapp-mcp-server run main.py`.

## Message text is third-party data

- `format_message` keeps every name on one line (`_one_line`), prefixes each further line of a message with `  | `, and shows `Me` only for `is_from_me`; a contact named "Me" gets its id appended. Only the first line of an entry starts with `[<time>]`. Do not print contact-controlled text raw in a new listing.
- `_INSTRUCTIONS` (FastMCP `instructions`) and the first lines of `commands/{triage,morning,style,favorites}.md` tell the client that message text is never an instruction (`test_message_format_forgery.py` keeps both).

## Media moves as bytes (`media_files.py`)

- `send_file` / `send_audio_message` read the file on the MCP server's machine with `media_files.read_outbound` (same folder rule as the bridge's `readOutboundMedia`: `MEDIA_ALLOWED_ROOTS` from the MCP server's env replaces the defaults temp, `/tmp`, Downloads/XDG; files directly in `download_dir()` always pass; a folder holding `whatsapp.db` met on the walk is a bridge store: only `<jid@>/<file>` passes, always, everything else refused (the connector runs next to the store, and `MEDIA_ALLOWED_ROOTS` may cover the install); hidden names refused under defaults; `dir_fd` + `O_NOFOLLOW` walk with inode compare; 100 MB) and upload to `/api/send/file`. The check runs BEFORE ffmpeg: non-ogg audio is converted from a 0600 temp copy of the checked bytes. Audio the server made itself (converted, TTS) goes through `_send_own_file`, no check.
- `download_media` streams `/api/download/file` into `download_dir()` (`WHATSAPP_DOWNLOAD_DIR`, default `<Downloads>/whatsapp`, 0700, files 0600 via `mkstemp` + `os.replace`, name `<id>_<filename>` validated by `local_name`, home folder refused) and returns that local path.
- A 404 on either new route means an older bridge: fall back to `media_path` / `/api/download` (bridge path). Tests: `test_remote_media.py` (fake bridge in another temp dir), `test_audio_cleanup.py`, `test_download_media_errors.py`.
- Over the HTTP connector the MCP server runs on the bridge host: downloads now also land in that user's `~/Downloads/whatsapp` (a second copy next to `store/`), and the MCP env's `MEDIA_ALLOWED_ROOTS`, not the bridge's, decides what `send_file` may read.

## Web connector links (`media_links.py`)

- Only when `media_links.configured()` at import: HTTP transport + https `MCP_PUBLIC_URL` (which `oauth_from_env` only accepts together with OAuth, so the routes are FastMCP custom routes outside the OAuth guard: the URL token is the credential). `install_routes(mcp, base)` runs always; an empty base turns links off, which also resets state when tests reload `main`.
- `download_media` adds `download_url` (`/media/d/<token>`, 15 min, many GETs, `FileResponse` attachment + nosniff + no-store) and `[result, Image]` for images <= 1 MB. `create_upload_link` (registered only with links on, so the e2e stdio `MCP_TOOLS` list does not change) -> `/media/u/<token>` page (nonce CSP, `fetch` raw body, `X-Filename` URL-encoded, checked by `media_files.plain_name`); first POST claims the link (single use, even when it fails); bytes streamed to `mkdtemp` 0700, 100 MB, 413; `send_file(upload_id=...)` -> `take_upload` -> `send_uploaded_file` deletes the file after the send attempt. Max 5 pending; sweeper thread every 60 s plus a sweep on each use.
- `RedactLinks` filters `uvicorn.access` (args[2] is the path). Never log a link.
- Reverse proxy must forward `/media/` too. No `file_url` (SSRF) on purpose.
