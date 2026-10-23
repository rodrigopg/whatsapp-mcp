"""Opt-in OAuth 2.1 for the HTTP transport.

Claude web connectors can only authenticate with OAuth (or a fixed header on some plans), so a
public MCP endpoint needs a real authorization server. This one is deliberately tiny: ONE owner,
no accounts. The owner proves who they are by typing a passphrase on the login page; everything
else is the standard flow (discovery, dynamic client registration, authorization code + PKCE,
refresh-token rotation, revocation) handled by the MCP SDK on top of this provider.

Enabled only when MCP_PUBLIC_URL (https) and MCP_OAUTH_PASSPHRASE are both set; otherwise nothing
changes. The static MCP_AUTH_TOKEN keeps working as a bearer token (Claude Code, request headers).
"""
import hashlib
import hmac
import html
import json
import os
import secrets
import sqlite3
import threading
import time
from typing import Callable, Optional
from urllib.parse import parse_qs, urlparse

from mcp.server.auth.provider import (
    AccessToken, AuthorizationCode, AuthorizationParams, RefreshToken, RegistrationError, construct_redirect_uri,
)
from mcp.server.auth.settings import AuthSettings, ClientRegistrationOptions, RevocationOptions
from mcp.shared.auth import OAuthClientInformationFull, OAuthToken
from starlette.requests import Request
from starlette.responses import HTMLResponse, JSONResponse, RedirectResponse

SCOPE = "whatsapp"
ACCESS_TTL = 3600
REFRESH_TTL = 30 * 86400
CODE_TTL = 300
PENDING_TTL = 600
MAX_FAILS, LOCK_WINDOW = 10, 900  # failed passphrases tolerated per window, across everyone
# Claude (claude.ai connectors) and ChatGPT (developer-mode connectors) callbacks; others are refused.
CLIENT_HOSTS = {"claude.ai", "www.claude.ai", "claude.com", "www.claude.com", "chatgpt.com", "chat.openai.com"}
LOOPBACK = {"localhost", "127.0.0.1", "[::1]", "::1"}


def redirect_allowed(uri: str) -> bool:
    """Only Claude's and ChatGPT's own callbacks (https) and loopback callbacks (Claude Code) may be registered."""
    u = urlparse(uri)
    return (u.scheme == "https" and u.hostname in CLIENT_HOSTS) or (u.scheme == "http" and u.hostname in LOOPBACK)


def _hash(token: str) -> str:
    return hashlib.sha256(token.encode()).hexdigest()


class OwnerOAuth:
    def __init__(self, db_path: str, passphrase: str, static_token: str, public_url: str, now: Callable[[], float] = time.time):
        self.passphrase, self.static_token, self.public_url, self.now = passphrase, static_token, public_url, now
        self._db = sqlite3.connect(db_path, check_same_thread=False)
        self._db.executescript(
            "CREATE TABLE IF NOT EXISTS clients (client_id TEXT PRIMARY KEY, body TEXT NOT NULL);"
            "CREATE TABLE IF NOT EXISTS tokens (hash TEXT PRIMARY KEY, kind TEXT NOT NULL, client_id TEXT NOT NULL,"
            " scopes TEXT NOT NULL, expires_at INTEGER NOT NULL);")
        self._lock = threading.Lock()
        self._pending: dict[str, tuple[float, str, AuthorizationParams]] = {}  # login id -> (expires, client_id, params)
        self._codes: dict[str, AuthorizationCode] = {}
        self._fails: list[float] = []

    # ---- clients ---------------------------------------------------------------
    async def get_client(self, client_id: str) -> Optional[OAuthClientInformationFull]:
        with self._lock:
            row = self._db.execute("SELECT body FROM clients WHERE client_id = ?", (client_id,)).fetchone()
        return OAuthClientInformationFull.model_validate_json(row[0]) if row else None

    async def register_client(self, client_info: OAuthClientInformationFull) -> None:
        for uri in client_info.redirect_uris:
            if not redirect_allowed(str(uri)):
                raise RegistrationError(error="invalid_redirect_uri", error_description="redirect_uri is not allowed for this server")
        with self._lock, self._db:
            self._db.execute("INSERT OR REPLACE INTO clients (client_id, body) VALUES (?, ?)", (client_info.client_id, client_info.model_dump_json()))

    # ---- authorization (the owner types the passphrase) -------------------------
    async def authorize(self, client: OAuthClientInformationFull, params: AuthorizationParams) -> str:
        req = secrets.token_urlsafe(24)
        with self._lock:
            self._pending = {k: v for k, v in self._pending.items() if v[0] > self.now()}
            self._pending[req] = (self.now() + PENDING_TTL, client.client_id, params)
        return f"{self.public_url}/oauth/login?req={req}"

    def locked(self) -> bool:
        with self._lock:
            self._fails = [t for t in self._fails if t > self.now() - LOCK_WINDOW]
            return len(self._fails) >= MAX_FAILS

    def check_passphrase(self, given: str) -> bool:
        ok = hmac.compare_digest(given.encode(), self.passphrase.encode())
        if not ok:
            with self._lock:
                self._fails.append(self.now())
        return ok

    def pending(self, req: str):
        with self._lock:
            entry = self._pending.get(req)
        return entry if entry and entry[0] > self.now() else None

    def approve(self, req: str) -> Optional[str]:
        """Turn a pending login into an authorization code and return the redirect back to the client."""
        with self._lock:
            entry = self._pending.pop(req, None)
        if not entry or entry[0] <= self.now():
            return None
        _, client_id, p = entry
        code = secrets.token_urlsafe(32)
        with self._lock:
            self._codes[code] = AuthorizationCode(code=code, scopes=p.scopes or [SCOPE], expires_at=self.now() + CODE_TTL, client_id=client_id,
                                                  code_challenge=p.code_challenge, redirect_uri=p.redirect_uri,
                                                  redirect_uri_provided_explicitly=p.redirect_uri_provided_explicitly)
        return construct_redirect_uri(str(p.redirect_uri), code=code, state=p.state)

    async def load_authorization_code(self, client: OAuthClientInformationFull, authorization_code: str) -> Optional[AuthorizationCode]:
        with self._lock:
            c = self._codes.get(authorization_code)
        return c if c and c.client_id == client.client_id and c.expires_at > self.now() else None

    async def exchange_authorization_code(self, client: OAuthClientInformationFull, authorization_code: AuthorizationCode) -> OAuthToken:
        with self._lock:
            self._codes.pop(authorization_code.code, None)  # single use
        return self._issue(client.client_id, authorization_code.scopes)

    # ---- tokens ----------------------------------------------------------------
    def _issue(self, client_id: str, scopes: list[str]) -> OAuthToken:
        access, refresh, now = secrets.token_urlsafe(32), secrets.token_urlsafe(32), int(self.now())
        with self._lock, self._db:
            self._db.execute("DELETE FROM tokens WHERE expires_at < ?", (now,))
            for tok, kind, ttl in ((access, "access", ACCESS_TTL), (refresh, "refresh", REFRESH_TTL)):
                self._db.execute("INSERT INTO tokens (hash, kind, client_id, scopes, expires_at) VALUES (?, ?, ?, ?, ?)",
                                 (_hash(tok), kind, client_id, json.dumps(scopes), now + ttl))
        return OAuthToken(access_token=access, token_type="bearer", expires_in=ACCESS_TTL, scope=" ".join(scopes), refresh_token=refresh)

    def _load(self, token: str, kind: str):
        with self._lock:
            row = self._db.execute("SELECT client_id, scopes, expires_at FROM tokens WHERE hash = ? AND kind = ?", (_hash(token), kind)).fetchone()
        return (row[0], json.loads(row[1]), row[2]) if row and row[2] > self.now() else None

    async def load_refresh_token(self, client: OAuthClientInformationFull, refresh_token: str) -> Optional[RefreshToken]:
        row = self._load(refresh_token, "refresh")
        return RefreshToken(token=refresh_token, client_id=row[0], scopes=row[1], expires_at=row[2]) if row and row[0] == client.client_id else None

    async def exchange_refresh_token(self, client: OAuthClientInformationFull, refresh_token: RefreshToken, scopes: list[str]) -> OAuthToken:
        with self._lock, self._db:
            self._db.execute("DELETE FROM tokens WHERE hash = ?", (_hash(refresh_token.token),))  # rotation
        return self._issue(client.client_id, scopes or refresh_token.scopes)

    async def load_access_token(self, token: str) -> Optional[AccessToken]:
        if self.static_token and hmac.compare_digest(token.encode(), self.static_token.encode()):
            return AccessToken(token=token, client_id="static-token", scopes=[SCOPE], expires_at=None)
        row = self._load(token, "access")
        return AccessToken(token=token, client_id=row[0], scopes=row[1], expires_at=row[2]) if row else None

    async def revoke_token(self, token) -> None:
        with self._lock, self._db:
            self._db.execute("DELETE FROM tokens WHERE hash = ?", (_hash(token.token),))


_PAGE = """<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Authorize WhatsApp MCP</title><style>body{{font:16px system-ui,sans-serif;max-width:26rem;margin:12vh auto;padding:0 1rem}}
input,button{{font:inherit;width:100%;padding:.6rem;margin:.4rem 0;box-sizing:border-box}}.err{{color:#b00020}}</style></head><body>
<h1>Authorize WhatsApp MCP</h1><p><b>{who}</b> wants to read and act on your WhatsApp through this server. After you allow it you are sent back to <b>{back}</b>. Type the passphrase to allow it.</p>
{error}<form method="post" action="/oauth/login"><input type="hidden" name="req" value="{req}">
<input type="password" name="passphrase" autocomplete="current-password" autofocus required placeholder="Passphrase">
<button type="submit">Allow</button></form></body></html>"""
_HEADERS = {"cache-control": "no-store", "x-frame-options": "DENY", "content-security-policy": "frame-ancestors 'none'", "referrer-policy": "no-referrer"}


def install_oauth_routes(mcp, provider: OwnerOAuth) -> None:
    """The login page and the RFC 9728 protected-resource metadata (the SDK serves the rest)."""

    async def page(req: str, status: int = 200, error: str = ""):
        entry = provider.pending(req)
        who, back = "an application", "?"
        if entry:
            client = await provider.get_client(entry[1])
            who = client.client_name if client and client.client_name else who
            back = urlparse(str(entry[2].redirect_uri)).netloc  # the client picks its own name; where it returns to is what to check
        err = f'<p class="err">{html.escape(error)}</p>' if error else ""
        return HTMLResponse(_PAGE.format(who=html.escape(who), back=html.escape(back), req=html.escape(req), error=err), status_code=status, headers=_HEADERS)

    @mcp.custom_route("/oauth/login", methods=["GET", "POST"])
    async def login(request: Request):
        if request.method == "GET":
            req = request.query_params.get("req", "")
            return await page(req) if provider.pending(req) else HTMLResponse("This login link has expired. Start again from Claude.", status_code=400, headers=_HEADERS)
        body = await request.body()
        if len(body) > 4096:
            return HTMLResponse("Request too large.", status_code=413, headers=_HEADERS)
        form = parse_qs(body.decode("utf-8", "replace"))
        req, given = (form.get("req") or [""])[0], (form.get("passphrase") or [""])[0]
        if provider.locked():
            return HTMLResponse("Too many wrong passphrases. Try again in 15 minutes.", status_code=429, headers=_HEADERS)
        # Every wrong passphrase counts, whatever the req: a valid req costs an attacker one /authorize call.
        if not provider.check_passphrase(given):
            return await page(req, 401, "Wrong passphrase.") if provider.pending(req) else HTMLResponse(
                "This login link has expired. Start again from Claude.", status_code=400, headers=_HEADERS)
        target = provider.approve(req)
        return RedirectResponse(target, status_code=302, headers=_HEADERS) if target else HTMLResponse("Expired.", status_code=400, headers=_HEADERS)

    async def prm(request: Request):
        return JSONResponse({"resource": provider.public_url + "/mcp", "authorization_servers": [provider.public_url],
                             "bearer_methods_supported": ["header"], "scopes_supported": [SCOPE]})

    for path in ("/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"):
        mcp.custom_route(path, methods=["GET"])(prm)


def oauth_from_env(env=os.environ):
    """(provider, AuthSettings), or None when OAuth is not configured. Half a configuration is an error."""
    url, pw = env.get("MCP_PUBLIC_URL", "").strip().rstrip("/"), env.get("MCP_OAUTH_PASSPHRASE", "")
    if not url and not pw:
        return None
    if not url or not pw:
        raise SystemExit("MCP_PUBLIC_URL and MCP_OAUTH_PASSPHRASE must be set together")
    if not url.startswith("https://"):
        raise SystemExit("MCP_PUBLIC_URL must be an https:// URL (the address Claude reaches, without /mcp)")
    if len(pw) < 24:
        raise SystemExit("MCP_OAUTH_PASSPHRASE must be at least 24 characters (for example: openssl rand -hex 24)")
    provider = OwnerOAuth(env.get("MCP_OAUTH_DB", "mcp-oauth.db"), pw, env.get("MCP_AUTH_TOKEN", ""), url)
    # resource_server_url=None: we serve the RFC 9728 metadata ourselves (install_oauth_routes).
    settings = AuthSettings(issuer_url=url, resource_server_url=None, required_scopes=[SCOPE],
                            client_registration_options=ClientRegistrationOptions(enabled=True, valid_scopes=[SCOPE], default_scopes=[SCOPE]),
                            revocation_options=RevocationOptions(enabled=True))
    return provider, settings
