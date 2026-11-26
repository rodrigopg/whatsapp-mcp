"""Temporary links for the remote HTTP connector (Claude web, ChatGPT), where the user is in a
browser and the MCP server runs next to the bridge: a path on that machine is useless to them.

- download_media adds a download link: GET /media/d/<token> serves that one saved file, as an
  attachment, for LINK_TTL seconds (several GETs allowed: browsers retry).
- create_upload_link gives an upload page: GET /media/u/<token> shows it, one POST of the raw file
  (at most media_files.MAX_MEDIA_BYTES) is accepted, the bytes go to a 0700 folder on disk, and the
  returned upload_id lets send_file send it once. The file is deleted after sending or when it
  expires; at most MAX_PENDING_UPLOADS exist at a time.

Tokens are 128-bit random, kept in memory only (a restart drops every link), and never logged:
uvicorn's access log is filtered (RedactLinks). Enabled only for the HTTP transport with an https
MCP_PUBLIC_URL; the link routes are reached without the bearer token, the token in the URL is the
credential.
"""

import logging
import os
import secrets
import tempfile
import threading
import time
import urllib.parse

from starlette.requests import Request
from starlette.responses import FileResponse, HTMLResponse, JSONResponse, Response

import media_files

LINK_TTL = 15 * 60
MAX_PENDING_UPLOADS = 5
PREFIX = "/media/"

_lock = threading.Lock()
_downloads = {}  # token -> (path, filename, expires)
_uploads = {}  # token -> dict(upload_id, expires, state "open"|"receiving"|"ready", path, filename)
_upload_dir = None

_HEADERS = {"cache-control": "no-store", "x-content-type-options": "nosniff", "referrer-policy": "no-referrer",
            "x-frame-options": "DENY"}


class LinkError(Exception):
    """A reason safe to return to the MCP client."""


_base = ""  # MCP_PUBLIC_URL once install_routes ran; empty = links off


def configured(env=os.environ):
    """Links are for the HTTP transport with an https MCP_PUBLIC_URL (read once, when main starts)."""
    url = env.get("MCP_PUBLIC_URL", "").strip().rstrip("/")
    http = env.get("MCP_TRANSPORT", "").strip().lower() == "streamable-http"
    return url if http and url.startswith("https://") else ""


def enabled():
    return bool(_base)


def _new_token():
    return secrets.token_urlsafe(16)


def _remove(path):
    if path:
        try:
            os.remove(path)
        except OSError:
            pass


def _sweep(now=None):
    """Drop expired links and delete expired uploads. Called on every use and by the sweeper thread."""
    now = time.time() if now is None else now
    with _lock:
        for t in [t for t, (_, _, exp) in _downloads.items() if exp <= now]:
            del _downloads[t]
        gone = [t for t, u in _uploads.items() if u["expires"] <= now and u["state"] != "receiving"]
        paths = [_uploads.pop(t).get("path") for t in gone]
    for p in paths:
        _remove(p)


def _sweeper():
    while True:
        time.sleep(60)
        _sweep()


_sweeper_started = False


def _start_sweeper():
    global _sweeper_started
    with _lock:
        if _sweeper_started:
            return
        _sweeper_started = True
    threading.Thread(target=_sweeper, daemon=True, name="media-links-sweeper").start()


def upload_dir():
    """A 0700 folder (random name) for this process's uploads."""
    # ponytail: a crashed process leaves its folder in the temp dir; uploads are deleted on send/expiry otherwise.
    global _upload_dir
    with _lock:
        if _upload_dir is None:
            _upload_dir = tempfile.mkdtemp(prefix="whatsapp-uploads-")
        return _upload_dir


def download_link(path, filename):
    """A link that serves this saved file for LINK_TTL seconds."""
    _start_sweeper()
    _sweep()
    token = _new_token()
    with _lock:
        _downloads[token] = (path, filename, time.time() + LINK_TTL)
    return f"{_base}{PREFIX}d/{token}"


def upload_link():
    """(url, upload_id) for a new single-use upload page."""
    _start_sweeper()
    _sweep()
    upload_dir()
    with _lock:
        if len(_uploads) >= MAX_PENDING_UPLOADS:
            raise LinkError(f"{MAX_PENDING_UPLOADS} upload links are already pending; use or let them expire "
                            f"({LINK_TTL // 60} minutes) first")
        token, upload_id = _new_token(), _new_token()
        _uploads[token] = {"upload_id": upload_id, "expires": time.time() + LINK_TTL, "state": "open"}
    return f"{_base}{PREFIX}u/{token}", upload_id


def take_upload(upload_id):
    """(path, filename) of an uploaded file, removed from the pending list: the caller sends it and
    deletes the file. LinkError when unknown, expired, or not uploaded yet."""
    _sweep()
    with _lock:
        for token, u in _uploads.items():
            if secrets.compare_digest(u["upload_id"], upload_id or ""):
                if u["state"] != "ready":
                    raise LinkError("nothing was uploaded with that link yet; ask the user to upload the file first")
                del _uploads[token]
                return u["path"], u["filename"]
    raise LinkError("unknown or expired upload_id; create a new upload link")


_PAGE = """<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>Send a file to WhatsApp</title>
<style>body{{font-family:system-ui,sans-serif;max-width:32rem;margin:3rem auto;padding:0 1rem}}
button{{font-size:1rem;padding:.5rem 1rem}}#s{{margin-top:1rem}}</style></head><body>
<h1>Send a file to WhatsApp</h1>
<p>Pick the file. After it is uploaded, go back to the chat: the assistant sends it when you ask.
This link works once and expires in {minutes} minutes. Up to {mb} MB.</p>
<input type="file" id="f"> <button id="b" type="button">Upload</button><p id="s" role="status"></p>
<script nonce="{nonce}">
document.getElementById("b").onclick = async () => {{
  const f = document.getElementById("f").files[0], s = document.getElementById("s");
  if (!f) {{ s.textContent = "Pick a file first."; return; }}
  s.textContent = "Uploading...";
  try {{
    const r = await fetch(location.href, {{method: "POST", body: f,
      headers: {{"X-Filename": encodeURIComponent(f.name), "Content-Type": "application/octet-stream"}}}});
    s.textContent = r.ok ? "Uploaded. You can go back to the chat." : "Upload failed: " + await r.text();
  }} catch (e) {{ s.textContent = "Upload failed."; }}
}};
</script></body></html>"""


def _gone():
    return Response("This link has expired or was already used.", status_code=404, headers=_HEADERS,
                    media_type="text/plain")


async def serve_download(request: Request):
    _sweep()
    with _lock:
        entry = _downloads.get(request.path_params["token"])
    if not entry:
        return _gone()
    path, filename, _ = entry
    if not os.path.isfile(path):
        return _gone()
    return FileResponse(path, filename=filename, media_type="application/octet-stream",
                        content_disposition_type="attachment", headers=_HEADERS)


async def upload_page(request: Request):
    _sweep()
    token = request.path_params["token"]
    with _lock:
        u = _uploads.get(token)
        state = u["state"] if u else None
    if request.method == "GET":
        if state != "open":
            return _gone()
        nonce = secrets.token_urlsafe(16)
        csp = (f"default-src 'none'; script-src 'nonce-{nonce}'; style-src 'unsafe-inline'; connect-src 'self'; "
               "form-action 'none'; frame-ancestors 'none'; base-uri 'none'")
        return HTMLResponse(_PAGE.format(minutes=LINK_TTL // 60, mb=media_files.MAX_MEDIA_BYTES >> 20, nonce=nonce),
                            headers={**_HEADERS, "content-security-policy": csp})
    return await _receive(request, token)


async def _receive(request, token):
    try:
        filename = media_files.plain_name(urllib.parse.unquote(request.headers.get("x-filename", ""), errors="strict"))
    except (media_files.MediaError, UnicodeDecodeError):
        return Response("the file name is not a plain file name", status_code=400, headers=_HEADERS)
    with _lock:  # single use: the first POST claims the link
        u = _uploads.get(token)
        if not u or u["state"] != "open" or u["expires"] <= time.time():
            return _gone()
        u["state"] = "receiving"
    fd, path = tempfile.mkstemp(dir=upload_dir(), prefix="up-")
    size, ok = 0, False
    try:
        with os.fdopen(fd, "wb") as f:
            async for chunk in request.stream():
                size += len(chunk)
                if size > media_files.MAX_MEDIA_BYTES:
                    return Response(f"the file is larger than {media_files.MAX_MEDIA_BYTES >> 20} MB",
                                    status_code=413, headers=_HEADERS)
                f.write(chunk)
        if size == 0:
            return Response("the file is empty", status_code=400, headers=_HEADERS)
        ok = True
    except OSError:
        return Response("the upload could not be saved", status_code=500, headers=_HEADERS)
    finally:
        with _lock:
            if ok and token in _uploads:
                _uploads[token].update(state="ready", path=path, filename=filename,
                                       expires=time.time() + LINK_TTL)
            else:
                _uploads.pop(token, None)
                ok = False
        if not ok:
            _remove(path)
    return JSONResponse({"uploaded": True}, headers=_HEADERS)


def install_routes(mcp, base):
    """Turn links on for base (configured()); an empty base turns them off and installs nothing."""
    global _base
    _base = base
    if not base:
        return
    mcp.custom_route(PREFIX + "d/{token}", methods=["GET"])(serve_download)
    mcp.custom_route(PREFIX + "u/{token}", methods=["GET", "POST"])(upload_page)
    logging.getLogger("uvicorn.access").addFilter(RedactLinks())


class RedactLinks(logging.Filter):
    """uvicorn logs the request path; a link token there would be a working credential in the log."""

    def filter(self, record):
        args = record.args
        if isinstance(args, tuple) and len(args) >= 3 and isinstance(args[2], str) and args[2].startswith(PREFIX):
            record.args = args[:2] + (PREFIX + "<link>",) + args[3:]
        return True
