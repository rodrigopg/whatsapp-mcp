# Contexto: Bridge Go

Bridge Go (`whatsapp-bridge/main.go`) — conecta ao WhatsApp via whatsmeow, expõe REST API local, persiste em SQLite (`store/messages.db`).

## Pontos críticos

- **whatsmeow exige `context.Context`** em todas as chamadas de API (quebra que congelou o upstream). Versão pinada: `v0.0.0-20260516102357-8d3700152a69`. Go **1.25+** obrigatório (`go.mod`).
- **REST API liga em `127.0.0.1` por padrão.** Upstream ligava `0.0.0.0` (qualquer um na LAN mandava mensagem como você). `BIND_ADDR=<ip>` (ex. `0.0.0.0`, ou um IP específico como um endereço Tailscale) reabre.
- **Porta** via `WHATSAPP_BRIDGE_PORT` (default 8080; este setup usa 8081).
- **`API_AUTH_TOKEN`**: se `BIND_ADDR` não é `127.0.0.1`/`localhost`, o processo **recusa subir**
  sem essa env var setada (fail-closed, não fail-open). Quando setada, todo `/api/*` exige
  header `Authorization: Bearer <token>` (middleware `apiGuard` em `api_guard.go`, aplicado só em
  `/api/*` — `/qr` e `/qr.png` ficam sempre abertos, é o próprio fluxo de pareamento).
- **`apiGuard` is the single gate of `/api/*`** (wraps the mux in `startRESTServer`; never add a
  per-handler check instead). Token set: constant-time bearer compare -> 401, nothing else (a
  page cannot send the header; a Host rule there would only break `BIND_ADDR=0.0.0.0` reached by
  LAN address, DNS name or another container). No token (bind is loopback): `Host` name must be
  loopback or in `API_ALLOWED_HOSTS` (port ignored on purpose: a container may publish another
  one) -> 403; any `Origin`, or `Sec-Fetch-Site` other than `none`/`same-origin` -> 403. The panel (`/status*`) is outside it and keeps its own
  cookie + same-origin + loopback-Host checks. Tests: `api_guard_test.go`.

## Endpoints REST de leitura (`/api/chats`, `/api/messages`, etc.)

- 9 endpoints somam-se aos de escrita já existentes: `/api/chats`, `/api/messages`,
  `/api/message_context`, `/api/contacts/search`, `/api/contacts/chats`,
  `/api/contacts/last_interaction`, `/api/chat`, `/api/chat/by_contact`, `/api/sender_name`.
  Contrato: os handlers em `whatsapp-bridge/main.go`.
- Motivo de existirem: o MCP Python lia esses dados via SQLite local direto; isso só funciona
  quando MCP e bridge rodam na mesma máquina. Bridge remota (VPS) exige que essas leituras
  também sejam REST — MCP server hoje é 100% stateless (ver `mcp-python.md`).
- `whatsapp.db` (store do whatsmeow) não expõe `*sql.DB` cru via `sqlstore.Container` — esses
  handlers abrem uma **segunda conexão SQLite read-only** própria só pra leituras de
  `whatsmeow_contacts`/`whatsmeow_lid_map`, separada da conexão de escrita gerenciada pela lib.
- Busca accent-insensitive (`unaccent`) migrou de Python pra um driver SQLite derivado
  (`sqlite3_unaccent`, registrado via `sql.Register`), usado só na conexão de leitura — não
  afeta a conexão de escrita principal.
- Timestamps de resposta usam offset explícito (`-03:00`), não `Z`/UTC — deliberado, pra
  compatibilizar com `datetime.fromisoformat` do Python <3.11.

## Captura de conteúdo

- `extractTextContent` retorna captions de image/video/document (`GetCaption()`). Bug original: caption era dropado na escrita → busca não achava texto que existia. Caminho live e caminho de history-sync compartilham a função (antes o sync tinha extractor inline duplicado com o mesmo bug).
- `StoreMessage` updates an existing row only for the same author (same `is_from_me`; for others' messages the same `sender`, the bare LID user counts as its LID JID, an empty sender on either side passes). Message IDs are chosen by the sender, so a reused ID from someone else is ignored and logged. Same rule as `ApplyEdit`.
- `storeMentions` writes one transaction, deduped, at most `maxStoredMentions` per message.
- The review cursor never passes now: `reviewMark` clamps `up_to`, and `reviewCursor` rewrites a stored future value to now on the first read.
- Mensagens enviadas são persistidas localmente na hora (echo multi-device não dispara em conta single-device).

## History sync

- `store.DeviceProps.RequireFullSync = proto.Bool(true)` + `HistorySyncConfig{FullSyncDaysLimit:365,...}` **antes de `NewClient`**. Sem isso só vem histórico recente. Recupera ~1 ano.
- **Armadilha:** `INSERT OR REPLACE INTO messages` (main.go:221) sobrescreve a linha inteira no sync. Re-parear/re-sync **apaga transcrições** (content volta a ''). COALESCE em messages ainda não existe (só em senders/chats). Backup `messages.db` antes de re-parear.

## LID→PN

- `resolveToPN` normaliza LID→PN na escrita (mesmo contato não racha em dois `chat_jid`).
- `migrateLIDChats` roda no startup, mescla rows `@lid` legados nos `@s.whatsapp.net` (transacional, idempotente).

## Mídia + segurança

- **`safeMediaPath`** (main.go ~889): rejeita componentes crus com `/`, `\`, `..`, `.`, vazio (não sanitiza — surface o ataque). IDs reais do WhatsApp são hex/base64 sem separadores.
- **`storePath`** (`store_dir.go`) is the only way to name a path in the store: `WHATSAPP_STORE_DIR` (absolute, checked at start by `checkStoreDir`) or `store/` when unset. SQLite opens go through `storeDSN`, which escapes the path. `store/` below means that folder.
- **`chatMediaDir`** is the only way to name a chat's media folder (`store/<chat id>`, one path component; a separator or `..` is refused). Request values printed to the log use `%q`, so a line break in them cannot start a fake log line (`recover_audios.py` parses the log).
- **`readOutboundMedia`** (`media_path.go`) is the only way `sendWhatsAppMessage` reads `media_path`. After `EvalSymlinks`: inside `store/` (matched by `os.SameFile`, so case-insensitive filesystems cannot dodge it) only `store/<jid>/<file>` passes, always; else the file must be inside a root: `MEDIA_ALLOWED_ROOTS` (path list) when set, otherwise `defaultMediaRoots()` = `os.TempDir()`, `/tmp` on Unix, `~/Downloads` and the `XDG_DOWNLOAD_DIR` of `user-dirs.dirs` (ignored when it covers the home). Hidden (`.`-prefixed) names below a default root are refused; a configured root allows them. Regular file; <= `maxMediaBytes` (100 MB).
  - The open is `openNoLinks(root, parts)`: an `os.Root` walk, one component at a time, `Lstat`, open, then `os.SameFile` on the opened handle. Any symlink or swapped component is refused, and so is reaching the store from an outer root. Never go back to `os.Stat` + `os.Open` by name (check-then-open race).
  - Errors to the caller are a `mediaRefusal` (reason, no path) or one of three fixed texts; never return an `*os.PathError`.
  - Traps: `t.TempDir()` is inside the real temp root, so tests replace the `defaultMediaRoots` variable (`mediaFixture`). The e2e launchers set `MEDIA_ALLOWED_ROOTS` (native: `.media`; Docker: `/media`). Since the upload route, the current MCP server no longer sends `media_path` (only to a bridge that 404s `/api/send/file`), so the shared-temp-folder trap for converted/TTS audio applies to old clients only.
- **Bytes over the API** (`media_transfer.go`), so a remote MCP server never needs a bridge path: `POST /api/send/file?recipient=&filename=[&message=]` with the raw file as body (no multipart: `ParseMultipartForm` spills big parts to the temp folder), `MaxBytesReader(maxMediaBytes)` -> 413, `checkUploadName` refuses non-plain names, bytes stay in memory, then `sendWhatsAppContent` (the media-free core `sendWhatsAppMessage` now calls after `readOutboundMedia`). `POST /api/download/file {message_id, chat_jid}` runs `downloadMedia` (DB lookup, `safeMediaPath`), opens `store/<jid>/<file>` through `openNoLinks`, streams it as `application/octet-stream` + nosniff + attachment, name in `X-Media-Filename` (`url.PathEscape`); errors are the fixed "Failed to download media" plus `reason: media_gone`. It is in `readOnlyAllowed`; `/api/send/file` is not. The MCP server falls back to the old routes on a 404 (bridge not yet rebuilt). Tests: `media_transfer_test.go`.
- **Permissions**: `store/` and chat folders 0700, files 0600; `tightenStore()` fixes older installs at startup. The pairing QR image is `store/whatsapp-qr.png` (`qrImagePath()`), removed on pairing.
- Path de mídia prefixado com message ID: filename derivado do sync-time colidia (571 áudios → 87 paths únicos), cache devolvia bytes errados.
- **Media retry** (recuperação de mídia expirada): `SendMediaRetryReceipt` → `events.MediaRetry` → `DecryptMediaRetryNotification` → `DownloadMediaWithPath`. History sync NÃO recupera (re-entrega as mesmas URLs mortas do CDN). Endpoint `/api/mediaretry`. Log contract estável: `MEDIA RETRY <id>: SUCCESS|NOTONPHONE|ERROR`.

## Testes

- `main_test.go` — `TestSafeMediaPath` (traversal + colisão de filename). `go test ./...`.

## Open matters, favorites, summary, drafts (stage 4)

Additive tables created in `NewMessageStore` (each feature owns its schema string): `matters` + `matter_events` + `review_cursor` (matters.go), `favorites` (favorites.go), `summaries` (summary.go), `drafts` (drafts.go), `message_mentions` (mentions.go). Each file has its `_test.go` and registers its routes from main.go with one line (`registerMatterRoutes`, `registerFavoriteRoutes`, `registerSummaryRoute`, `registerDraftRoutes`); handlers use the generic `matterRoute`.

- Matter state is **derived** (`matterState`): open, closed_confirmed, closed_owner, expired (one month after it arose or was reopened). No background job. `needsAttention` holds the "when does it ask for attention" table; the day is read in the zone of `as_of` (the bridge never decides a timezone).
- Windows compare **instants** with `julianday(...)`, never timestamp strings: `messages.timestamp` keeps whatever offset it was written with.
- Rules the code enforces: the owner's own messages never close a matter; groups only with the owner's @mention (`checkTrackedChat`, `mentionsOwner`, owner = phone and LID); status/newsletters/own chat never tracked; closing needs a message newer than the matter.
- `approveDraft` is the **only** path that sends a chase: it claims the draft (`written` -> `sending`) before `sendWhatsAppMessage` and sends exactly the text it receives. The "only after the owner decided" guard is prompt-level (docstring + `/whats:morning`); the server cannot prove a human saw the text. A failed send puts the draft back to pending.
- The summary is a stored photograph per day (`summaries`); announcing a closure clears `announce_pending`.
- WhatsApp Favorites are mirrored from `events.AppState` (index `favorites`); `EmitAppStateEventsOnFullSync` is off, so a list that existed before pairing is picked up on its next change.
- Local checks run in a Go container (no Go on the Mac): mount the repo, `go vet ./... && go test ./...`; `gofmt` also rewrites `webhook.go` (known nit), do not commit that.

## Deleting a chat (e2e cleanup)

`POST /api/chat/delete` (`chat_delete.go`) deletes a chat from WhatsApp through the `deleteChat` app-state patch (with the stored last message as range). It refuses unless the body carries `expect_name_prefix` and the chat's stored name starts with it, so a mistyped JID cannot delete a real conversation; `forget_local` also drops the local rows. REST only, no MCP tool on purpose (an AI client should not be able to delete chats). Used by the live gate to remove its `e2e-...` groups.

## App-state drift ("mismatching LTHash")

If the log shows `failed to verify patch vNN: mismatching LTHash` and every archive / mark-read / delete-chat answers 500 "server returned error updating app state", the local copy of that app-state collection drifted (this happened on the personal bridge; it also explains the e2e gate skipping app-state calls for account B). Repair: `POST /api/appstate/resync {"name":"regular_low"}` (`appstate_resync.go`), a whatsmeow full sync of that collection (cached state dropped, refetched); when the server's own patches no longer verify even that fails, and the repair is `{"name":"regular_low","mode":"recovery"}`, which asks the phone (primary device, must be online) for a fresh copy of the collection through `BuildAppStateRecoveryRequest`; whatsmeow processes the answer by itself. Names: regular, regular_low, regular_high, critical_block, critical_unblock_low. Archive uses regular_low.

## Status panel (`panel.go`)

`/status` is the onboarding panel (Portuguese UI, stdlib `html/template`, no JS framework). The open page is read-only; the connector link (`MCP_PUBLIC_URL` + `/mcp`), **Reconectar**, the connector check and "último acesso à API" render only for a signed-in request.

- Panel key = `API_AUTH_TOKEN`. `POST /status/login` sets cookie `panel` = HMAC(token, "panel"), HttpOnly, SameSite=Strict, Path=/status. No token configured: unlocked only for a loopback `Host` (DNS rebinding cannot fake it). 10 wrong keys per 15 min lock sign-in for the client address that typed them (`clientKey`: peer address, /64 for IPv6, and the last `X-Forwarded-For` entry only when the peer is loopback, never from another peer; at most `loginMaxClients` tracked, then a shared count). Panel form bodies are capped at `panelMaxBody` in `login` and `allowed`. The HTTP server is `newRESTServer` (read timeouts, no write timeout on purpose).
- Every action (`/status/reconnect`, `/status/diagnose`, `/status/logout`) goes through `panel.action`: POST, `Origin`/`Referer` host equal to `Host`, signed in. Add new actions through it, never as a bare handler.
- `panelState` picks the one banner (ok, stall, pairing, qr, loggedout, held, stopped, offline, waiting). Stall = connected and newest stored message older than the setting (default 6 h, 0 = never). `store/logged_out` is a marker written on `events.LoggedOut` (it survives the restart) and removed when a QR is scanned.
- `connectionSupervisor.forceReconnect` = Disconnect + Connect of the same session; refused when held/stopped/unpaired and within 2 minutes of the previous one. The watchdog `reconnect()` is a no-op while the socket looks connected, which is the silent-stall case.
- `diagnoseConnector` fetches `MCP_PUBLIC_URL/.well-known/oauth-protected-resource/mcp` from the bridge host: proves DNS, certificate, tunnel and MCP server with OAuth; not the vendor's network path nor the passphrase.
- Screenshots: the panel renders from a `statusInfo` func, so a throwaway `_test.go` serving `newPanel(...).page` with fake states is enough; do not start a real bridge for it.
- **Insights** (`insights.go`, `/status/insights`): signed-in only, GET only, `?d=7|30|90` and `?hide=1`. `loadInsights` computes everything per request from `messages.db` (days and hours are BRT via `strftime(..., '-3 hours')`; status and newsletter chats excluded; reply time = median gap between someone's message and the owner's next one in direct chats; "waiting for you" = open matters owed by the owner). Charts are server-rendered SVG/CSS with native `<title>` tooltips, no JS. `chatLabel` masks a chat that has no stored name, so a full phone number never reaches the page. The page shares the panel's CSS and nav through the `css` and `nav` named templates in `panelTmpl`; a new panel page is `panelTmpl.New("<name>")` plus one line in `panel.register`.
- **Settings** (`settings.go`, `/status/settings`): `store/settings.json` (0600, written tmp + rename, loaded once in `main`; a broken file stops the start on purpose) holds what the web may change; `appSettings.get()` is nil-safe and returns defaults. Every change goes through `panel.action` or `panel.allowed`, and every form value is validated before `save`. Never render a secret: the transcription key is write-only and a generated panel key is shown once, stored as SHA-256.
  - Read-only mode is enforced by `readOnlyGuard` in front of the mux, not in the MCP server: `/api/` routes missing from `readOnlyAllowed` get 403. A new **read** route must be added to that map or it is refused in read-only mode; a new write route needs nothing.
  - Panel key: `keyed`/`keyOK`/`cookieValue`. A generated key replaces `API_AUTH_TOKEN` for sign-in only (the API bearer token is untouched); `panel_epoch` is part of the cookie HMAC, so bumping it signs every browser out.
  - Connected apps: the bridge opens the remote connector's SQLite (`MCP_OAUTH_DB`, tables `clients` and `tokens` created by `whatsapp-mcp-server/oauth.py`) and deletes a client's tokens; the connector looks tokens up per request, so it takes effect at once. A schema change there must be mirrored in `listConnectedApps`.
  - Transcription: the sweep skips when switched off and appends `settings.transcriptionEnv()` last to the child's environment (last value wins).
  - Unpair: `connectionSupervisor.unpair` needs a live connection, calls whatsmeow `Logout`, then exits through `exitAfterLogout` like a server-side logout, without the `store/logged_out` marker.
