# Onboarding panel: reconnect, connector link, "can my AI reach me?"

Next slice of the bridge `/status` page (`whatsapp-bridge/status.go`). Goal: a person with no
technical background can check, fix and connect their WhatsApp to Claude or ChatGPT from one page,
without a terminal.

## What stays as is

- `/status` keeps working without a token and keeps showing only what it shows today: connection
  state, QR while pairing, masked account, uptime, message count, newest message (BRT).
- `/qr`, `/qr.png`, `/healthz` unchanged. `/api/*` keeps the bearer token.

## New elements

| Element | Who sees it | What it does |
|---|---|---|
| State banner | everyone | One of the states below, with one plain sentence of what to do. |
| **Reconectar** button | signed-in only | Forced reconnect of the existing session (disconnect + connect). Never unpairs, never shows a new QR. |
| **Link do conector** + **Copiar** | signed-in only | `MCP_PUBLIC_URL` + `/mcp` in a read-only field and a copy button; two short how-to blocks (Claude, ChatGPT). |
| **Testar conexão com a IA** | signed-in only | On-demand check (below), result shown green or red with what it proves. |
| Sign-in form | everyone, when signed out | One field: the panel key. |

## Auth model

- **Panel key = `API_AUTH_TOKEN`.** No new secret. Typed once in the sign-in form
  (`POST /status/login`); the bridge sets cookie `panel` = HMAC-SHA256(token, "panel"),
  `HttpOnly`, `SameSite=Strict`, `Secure` when the request came over HTTPS (`X-Forwarded-Proto` or
  TLS), `Path=/status`, 30 days. The raw token never goes into the cookie.
- **Every state-changing route is `POST`** (`/status/reconnect`, `/status/diagnose`,
  `/status/logout`) and checks, in this order: cookie valid (constant-time compare), and
  `Origin` (or `Referer` when Origin is missing) has the same host as the request. Anything else:
  403, nothing done. `SameSite=Strict` + Origin check is the CSRF defence; no hidden form token.
- **No token configured** (default loopback install): the panel is unlocked without sign-in, but
  only when `Host` is `localhost`, `127.0.0.1` or `[::1]` (blocks DNS rebinding) and the Origin
  check passes (blocks a web page posting to the local bridge). Any other Host: panel stays locked
  and says why.
- **Brute force:** at most 10 failed sign-ins per 15 minutes across everyone, then 429 until the
  window passes (same policy as the MCP OAuth login).
- The connector link and buttons are rendered only for a signed-in request; the open page never
  contains the link, not even hidden.
- The MCP OAuth passphrase is never shown or known by the bridge. The panel only says the
  connector will ask for "a senha do conector" set during install.

## States (each maps to a signal the bridge can observe)

| State | Signal | Banner | Reconnect |
|---|---|---|---|
| Connected | `IsConnected() && IsLoggedIn()`, newest message < 6 h old | green "Conectado" | enabled |
| Silent stall | connected, but newest stored message ≥ 6 h old (parsed `MAX(timestamp)`) | amber "Conectado, mas nenhuma mensagem nova há N h. Pode ser normal se ninguém escreveu; se estranhar, reconecte." | enabled, highlighted |
| Disconnected, QR | `qrState.png != nil` | "Escaneie para conectar" + QR + 3 steps | hidden |
| Pairing | QR success seen in this run, and the last `events.HistorySync` chunk arrived < 2 min ago (or none yet) | "Conectado! Baixando seu histórico, pode levar alguns minutos." | hidden |
| Logged out by WhatsApp | marker file `store/logged_out` (time + reason) written on `events.LoggedOut` before the exit; deleted on QR success | red "O WhatsApp desconectou este aparelho em DD/MM HH:MM. Escaneie de novo." + QR when ready | hidden |
| Disconnected, paired | paired, not connected, no QR | red "Sem conexão. Tentando de novo sozinho." | enabled unless held |
| Held | `supervisor.held()` (temporary ban, second client) or `isStopped()` (outdated client) | reason in plain words, "volte em N min" | disabled, reason shown |

Auto-refresh (`meta refresh`, 10 s) only in the QR and pairing states; the other states do not
reload, so a typed key, the copy field and a diagnostic result are not wiped.

Stall threshold: 6 h constant. `MAX(timestamp)` is the message's own time, so a backfill of old
messages does not move it forward, and a quiet account at night is normal: the wording says so.

## Reconnect

- `connectionSupervisor.forceReconnect()`: refuses when `isStopped()` or `held()`; otherwise
  `Disconnect()` then `Connect()`. The existing `reconnect()` is a no-op while connected, which
  is exactly the silent-stall case, so it cannot be reused as is.
- Cooldown: one forced reconnect per 2 minutes (repeated connects are a ban risk). During the
  cooldown the button is disabled with "aguarde N s".
- `startRESTServer` receives the supervisor.
- No unpair / switch-number button in this slice.

## Connector link

- The bridge reads `MCP_PUBLIC_URL`, the same variable the MCP server uses for OAuth, so one value
  feeds both. Link shown = `MCP_PUBLIC_URL` + `/mcp` (the MCP server serves `/mcp` and `/mcp/`).
- Not set: the block says "Conector remoto não configurado" and points to the README section.
- One link serves both apps (the MCP OAuth server accepts Claude and ChatGPT callbacks). One copy
  button; two how-to blocks. Menu paths for each app are verified before build, not written from
  memory.
- Copy uses `navigator.clipboard` (a few lines of inline JS) with the read-only field selected on
  focus as fallback (clipboard needs HTTPS or localhost).

## "Testar conexão com a IA" (diagnostic)

Runs on click (`POST /status/diagnose`, signed-in, 5 s timeout, at most once per 10 s). Two lines:

1. **Endereço público responde.** `GET MCP_PUBLIC_URL/.well-known/oauth-protected-resource/mcp`
   from the bridge. Green when 200 with JSON naming the same resource. Proves: DNS, TLS
   certificate, tunnel or reverse proxy, MCP server running with OAuth. Does not prove: that
   Claude's or ChatGPT's servers reach it from their network, that the passphrase is right. When
   bridge and MCP server share a host, the request leaves and comes back through the public
   address, so it is still a real external-path check, just from one vantage point.
2. **Último acesso à API.** Time of the last authenticated `/api/*` request. Labelled "último
   acesso à API", not "último uso pelo Claude": transcription jobs use the same token.

Red results say the likely cause in one line (no URL configured, DNS, certificate, tunnel down,
MCP server down, OAuth not enabled).

## Reuse

`status.go` template and `statusInfo`, `maskAccount`, `lastMessage` (extended to also return the
parsed time), `qrState`, `connectionSupervisor` (`held`, `isStopped`), the `requireBearerToken`
wrapper point in `startRESTServer` (to stamp the last API access), stdlib `html/template`,
`crypto/hmac`, `net/http`. No new dependency, no JS framework.

## Tests (new `panel_test.go`)

- POST routes: no cookie → 403; wrong Origin → 403; valid → 200/303. No-token mode: Host
  `evil.example` → locked.
- Open page never contains the connector URL; signed-in page does.
- Each state renders its banner; refresh tag only in QR and pairing.
- Sign-in rate limit.
- `forceReconnect` refuses when held/stopped and within cooldown (fake session, like
  `lifecycle_test.go`).
- Diagnostic against an `httptest` server: 200 → green, 404 / timeout / bad JSON → red.

## Out of scope (parked)

- `/api/*` is CSRF-reachable from a browser in no-token loopback mode (pre-existing).
- Unpair / switch number.
- An identifying header from the MCP server to tell AI use from transcription jobs.
