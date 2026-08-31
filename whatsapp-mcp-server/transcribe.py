"""Backfill audio transcriptions into messages.content.

Headless transcription pipeline (local whisper.cpp or an OpenAI-compatible API).
For each audio message with empty content, it downloads the media through the
bridge, verifies the bytes against the stored plaintext SHA-256 as an integrity
check (files are identified on disk by their `<messageID>_` path prefix, written
by the bridge; the SHA guards against a corrupt or mismatched download), runs
the configured engine, and writes the result back into messages.content so the
normal accent-insensitive search finds it.

Idempotency uses three distinct content states:
  - real text         -> transcribed, done
  - SENTINEL_* marker  -> done but no usable transcript (empty audio / unavailable)
  - '' (empty)         -> not yet processed (a crash resumes from here)

Run:  python3 transcribe.py            # backfill every pending audio
      python3 transcribe.py --limit 5  # process only N (smoke test)
"""

import argparse
import hashlib
import json
import math
import os
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import time
from datetime import datetime, timedelta, timezone

import requests

from db_path import resolve_messages_db

# WhatsApp purges undelivered media from its CDN after roughly 2-3 weeks. Past
# this age a CDN download failure is permanent (the media may still be
# recoverable from the phone via recover_audios.py); before it, treat the
# failure as transient and let the next sweep retry.
CDN_EXPIRY = timedelta(days=21)

# Same resolution chain the rest of the server uses (WHATSAPP_MESSAGES_DB, then
# repo-relative, then ~/.whatsapp-mcp) instead of hardcoding the repo layout —
# a sweep pointed at the wrong file finds no pending audio and reports success.
# recover_audios.py imports DB_PATH from here, so it inherits the same path.
DB_PATH = resolve_messages_db()
API_BASE = os.environ.get("WHATSAPP_API_BASE_URL", f"http://localhost:{os.environ.get('WHATSAPP_BRIDGE_PORT', '8080')}/api")
WHATSAPP_API_AUTH_TOKEN = os.environ.get("WHATSAPP_API_AUTH_TOKEN", "")


def _bridge_auth_headers():
    if WHATSAPP_API_AUTH_TOKEN:
        return {"Authorization": f"Bearer {WHATSAPP_API_AUTH_TOKEN}"}
    return {}

# --- Transcription engine configuration (all via env so the repo is portable) ---
#
# Engine selection: TRANSCRIPTION_ENGINE = "local" (whisper.cpp) | "api" (OpenAI/Groq).
# Defaults reproduce the original author's local setup exactly, so an existing
# install keeps working with no env changes. A fresh clone with neither engine
# configured does nothing (see engine_ready / main) rather than marking audios.
TRANSCRIPTION_ENGINE = os.environ.get("TRANSCRIPTION_ENGINE", "local").lower()

# Local backend (whisper.cpp)
WHISPER_CLI = os.environ.get("WHISPER_CLI", "/Users/rodrigo/git/whisper.cpp/build/bin/whisper-cli")
WHISPER_MODEL = os.environ.get("WHISPER_MODEL", "/Users/rodrigo/PyCharmMiscProject/models/ggml-medium.bin")
DECODING_OPTS = ["--temperature", "0", "--no-fallback", "--max-context", "0", "--split-on-word"]

# API backend (OpenAI Whisper, or any OpenAI-compatible endpoint such as Groq).
# Set TRANSCRIPTION_API_KEY to enable. Override base/model for Groq:
#   TRANSCRIPTION_ENGINE=api TRANSCRIPTION_API_KEY=gsk_...
#   TRANSCRIPTION_API_BASE=https://api.groq.com/openai/v1 TRANSCRIPTION_API_MODEL=whisper-large-v3
TRANSCRIPTION_API_KEY = os.environ.get("TRANSCRIPTION_API_KEY", "")
TRANSCRIPTION_API_BASE = os.environ.get("TRANSCRIPTION_API_BASE", "https://api.openai.com/v1")
TRANSCRIPTION_API_MODEL = os.environ.get("TRANSCRIPTION_API_MODEL", "whisper-1")
API_MAX_BYTES = 25 * 1024 * 1024  # OpenAI endpoint hard limit

# Shared prompt — biases both engines toward correct PT-BR punctuation + TOTVS terms.
WHISPER_PROMPT = os.environ.get(
    "TRANSCRIPTION_PROMPT",
    "A seguir, a transcrição de um áudio. A transcrição deve ser precisa, com "
    "pontuação e capitalização corretas. Nomes próprios como PROTHEUS, PIMS, "
    "ADVPL, TOTVS devem ser mantidos em maiúsculas.",
)


def engine_ready():
    """Return (ok, reason). False means the engine isn't configured — callers
    must do NOTHING (leave content='') rather than mark audios, so enabling
    transcription later still picks them up."""
    if TRANSCRIPTION_ENGINE == "local":
        if not os.path.exists(WHISPER_CLI):
            return False, f"local engine: whisper-cli not found at {WHISPER_CLI}"
        if not os.path.exists(WHISPER_MODEL):
            return False, f"local engine: model not found at {WHISPER_MODEL}"
        if not shutil.which("ffmpeg"):
            return False, "local engine: ffmpeg not found"
        return True, "local"
    if TRANSCRIPTION_ENGINE == "api":
        if not TRANSCRIPTION_API_KEY:
            return False, "api engine: TRANSCRIPTION_API_KEY not set"
        return True, "api"
    return False, f"unknown TRANSCRIPTION_ENGINE={TRANSCRIPTION_ENGINE!r} (use 'local' or 'api')"

# Sentinels mark "done, but no searchable text" so they are never retried.
SENTINEL_EMPTY = "[áudio sem transcrição]"
SENTINEL_UNAVAILABLE = "[áudio indisponível: mídia expirada no servidor]"


def log(msg):
    print(f"[{time.strftime('%H:%M:%S')}] {msg}", flush=True)


def _is_expired(ts):
    """True if the message timestamp is older than the CDN retention window."""
    if not ts:
        return True  # unknown age — assume old, don't retry forever
    try:
        dt = datetime.fromisoformat(str(ts))
    except ValueError:
        return True
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return datetime.now(timezone.utc) - dt > CDN_EXPIRY


def pending_audios(conn, limit=None):
    sql = ("SELECT id, chat_jid, hex(file_sha256), timestamp FROM messages "
           "WHERE media_type='audio' AND (content IS NULL OR content='') "
           "ORDER BY timestamp DESC")
    if limit:
        sql += f" LIMIT {int(limit)}"
    return conn.execute(sql).fetchall()


def download(message_id, chat_jid):
    """Download via the bridge. Returns (path, error, reached_bridge).

    On success error is None; on failure path is None and error carries the
    bridge's message so the caller can distinguish an expired 403 from a bridge
    bug in the log. reached_bridge is False when the request never got an answer
    at all (bridge down, wrong port, connection refused) — the caller must not
    conclude anything about the media in that case."""
    try:
        r = requests.post(f"{API_BASE}/download",
                          json={"message_id": message_id, "chat_jid": chat_jid},
                          headers=_bridge_auth_headers(),
                          timeout=120)
    except requests.RequestException as e:
        return None, f"request error: {e}", False
    try:
        body = r.json()
    except ValueError:
        return None, f"HTTP {r.status_code}: {r.text[:120]}", True
    if r.status_code == 200 and body.get("success"):
        return body.get("path"), None, True
    return None, body.get("message", f"HTTP {r.status_code}"), True


def sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(65536), b""):
            h.update(chunk)
    return h.hexdigest().upper()


def transcribe(ogg_path, message_id):
    """Transcribe one audio file to text. Dispatches to the configured engine.
    Public signature is stable — recover_audios.py imports and calls this."""
    if TRANSCRIPTION_ENGINE == "api":
        return _transcribe_api(ogg_path)
    return _transcribe_local(ogg_path, message_id)


def _transcribe_local(ogg_path, message_id):
    """ffmpeg -> wav -> whisper-cli -> text. Unique temp names per message."""
    tmpdir = tempfile.mkdtemp(prefix=f"wa_tx_{message_id}_")
    wav = os.path.join(tmpdir, "audio.wav")
    out_base = os.path.join(tmpdir, "out")
    try:
        subprocess.run(["ffmpeg", "-i", ogg_path, "-ar", "16000", "-ac", "1",
                        "-c:a", "pcm_s16le", "-y", wav],
                       check=True, capture_output=True)
        cmd = [WHISPER_CLI, "-m", WHISPER_MODEL, "-l", "pt", "-oj", "-of", out_base,
               "-f", wav, "--prompt", WHISPER_PROMPT] + DECODING_OPTS
        subprocess.run(cmd, check=True, capture_output=True)
        with open(out_base + ".json", encoding="utf-8") as f:
            data = json.load(f)
        text = (data.get("text") or "").strip()
        if not text:
            text = " ".join(s.get("text", "") for s in data.get("transcription", [])).strip()
        return text
    finally:
        shutil.rmtree(tmpdir, ignore_errors=True)


class FatalTranscriptionError(Exception):
    """A misconfiguration (e.g. bad API key) that retrying won't fix — abort the run."""


API_MAX_RATE_LIMIT_RETRIES = 5


def _retry_after_seconds(retry_after_header, attempt):
    """Groq/OpenAI send Retry-After in seconds on 429. Fall back to exponential
    backoff (2^attempt) if the header is absent, not a plain finite number, or
    out of a sane range — never let a malformed/hostile header hang or crash
    the retry loop (float() alone accepts "inf"/"nan"/negative values, which
    would reach time.sleep as an OverflowError/ValueError or a no-op wait).
    Clamped to 60s so a legitimate but huge Retry-After (e.g. daily quota
    reset) doesn't stall a backfill for the full duration unnoticed."""
    if retry_after_header is not None:
        try:
            value = float(retry_after_header)
            if math.isfinite(value):
                return max(0.0, min(value, 60.0))
        except ValueError:
            pass
    return float(2 ** attempt)


def _transcribe_api(ogg_path):
    """OpenAI-compatible STT (/audio/transcriptions). Serves OpenAI or Groq via env.

    Groq's free tier rate-limits aggressively enough that a backfill of a few
    hundred audios hits 429 well before finishing. Respect Retry-After (Groq
    sends it in seconds) and back off; this is the same audio retried, not the
    next sweep, since a 429 says nothing about whether this specific file is OK.
    """
    size = os.path.getsize(ogg_path)
    if size > API_MAX_BYTES:
        # Don't silently drop — voice notes are tiny, so this is rare; surface it.
        raise RuntimeError(f"audio {size} bytes exceeds API limit {API_MAX_BYTES}")

    for attempt in range(API_MAX_RATE_LIMIT_RETRIES + 1):
        with open(ogg_path, "rb") as f:
            r = requests.post(
                f"{TRANSCRIPTION_API_BASE}/audio/transcriptions",
                headers={"Authorization": f"Bearer {TRANSCRIPTION_API_KEY}"},
                files={"file": (os.path.basename(ogg_path), f, "audio/ogg")},
                data={"model": TRANSCRIPTION_API_MODEL, "language": "pt", "prompt": WHISPER_PROMPT},
                timeout=120,
            )
        # A rejected key fails every audio identically — don't loop on it forever.
        if r.status_code in (401, 403):
            raise FatalTranscriptionError(
                f"transcription API rejected the key (HTTP {r.status_code}); check TRANSCRIPTION_API_KEY")
        if r.status_code == 429:
            if attempt == API_MAX_RATE_LIMIT_RETRIES:
                # Still rate-limited after backing off — this isn't a bad audio,
                # it's a still-active rate limit. Abort the whole run (same as a
                # rejected key) instead of burning the rest of the queue one
                # retry-and-fail cycle at a time against a limit that isn't lifting.
                raise FatalTranscriptionError(
                    f"still rate limited after {API_MAX_RATE_LIMIT_RETRIES} retries (HTTP 429); "
                    "the API's rate limit isn't clearing — wait and re-run, or check your plan/quota")
            wait = _retry_after_seconds(r.headers.get("Retry-After"), attempt)
            log(f"  rate limited (429), waiting {wait:.0f}s before retry {attempt + 1}/{API_MAX_RATE_LIMIT_RETRIES}")
            time.sleep(wait)
            continue
        r.raise_for_status()
        return (r.json().get("text") or "").strip()


def write_content(message_id, chat_jid, content):
    """Short-lived write so we never hold the DB during transcription."""
    conn = sqlite3.connect(DB_PATH, timeout=10)
    try:
        conn.execute("PRAGMA busy_timeout=5000")
        conn.execute("UPDATE messages SET content=? WHERE id=? AND chat_jid=?",
                     (content, message_id, chat_jid))
        conn.commit()
    finally:
        conn.close()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--limit", type=int, default=None)
    args = ap.parse_args()

    # If no engine is configured, do nothing and touch zero rows. "Not
    # configured" is not "failed" — marking audios here would permanently skip
    # them once the user later enables transcription. Exit 0 so the bridge
    # sweep treats it as a clean no-op.
    ok, reason = engine_ready()
    if not ok:
        log(f"Transcription not active ({reason}); leaving audios untouched.")
        return
    log(f"Engine: {reason}")

    conn = sqlite3.connect(DB_PATH, timeout=10)
    rows = pending_audios(conn, args.limit)
    conn.close()

    total = len(rows)
    log(f"Pending audios: {total}")
    done = empty = unavailable = mismatch = failed = 0

    for i, (msg_id, chat_jid, exp_sha, ts) in enumerate(rows, 1):
        prefix = f"[{i}/{total}] {msg_id[:12]}"
        try:
            # Force a fresh download: drop this message's own cached file so the
            # bridge re-fetches rather than serving a possibly-stale copy.
            chat_dir = os.path.join(os.path.dirname(DB_PATH), chat_jid.replace(":", "_"))
            if os.path.isdir(chat_dir):
                for fn in os.listdir(chat_dir):
                    if fn.startswith(msg_id + "_"):
                        try:
                            os.remove(os.path.join(chat_dir, fn))
                        except OSError:
                            pass

            path, dl_err, reached_bridge = download(msg_id, chat_jid)
            if not path or not os.path.isfile(path):
                # Only mark permanently unavailable once the CDN window has
                # certainly passed AND the bridge actually answered. A failure
                # that never reached the bridge (it was restarted mid-sweep,
                # wrong port, connection refused) says nothing about the media:
                # marking it expired would retire a perfectly good audio for
                # good, since the sentinel is never retried. Observed for real —
                # a sweep kept running after the bridge was stopped and wrote 43
                # false "expired" markers in a couple of minutes.
                # A recent audio that fails is likewise left at content='' so the
                # next sweep retries instead of silently losing a live message.
                if reached_bridge and _is_expired(ts):
                    write_content(msg_id, chat_jid, SENTINEL_UNAVAILABLE)
                    unavailable += 1
                    log(f"{prefix} unavailable (expired CDN): {dl_err}")
                else:
                    failed += 1
                    log(f"{prefix} download failed but recent — will retry next sweep: {dl_err}")
                continue

            actual = sha256_file(path)
            if exp_sha and actual != exp_sha:
                # Wrong bytes — do NOT write a transcript that would be misattributed.
                mismatch += 1
                log(f"{prefix} SHA MISMATCH expected={exp_sha[:12]} got={actual[:12]} — skipping")
                continue

            text = transcribe(path, msg_id)
            if text:
                write_content(msg_id, chat_jid, text)
                done += 1
                log(f"{prefix} ok ({len(text)} chars): {text[:60]}...")
            else:
                write_content(msg_id, chat_jid, SENTINEL_EMPTY)
                empty += 1
                log(f"{prefix} empty audio (no speech)")
        except FatalTranscriptionError as e:
            # Misconfiguration — every audio would fail the same way. Stop now
            # rather than burning the whole worklist (and API requests) on it.
            log(f"{prefix} FATAL: {e}")
            log("Aborting run — fix the configuration and re-run.")
            break
        except subprocess.CalledProcessError as e:
            # ffmpeg / whisper-cli failed — surface stderr, not just the exit code.
            failed += 1
            stderr = (e.stderr or b"")
            if isinstance(stderr, bytes):
                stderr = stderr.decode("utf-8", "ignore")
            log(f"{prefix} ERROR ({e.cmd[0]} exit {e.returncode}): {stderr[-300:].strip()}")
        except Exception as e:
            failed += 1
            log(f"{prefix} ERROR: {e!r}")

    log(f"DONE. transcribed={done} empty={empty} unavailable={unavailable} "
        f"sha_mismatch={mismatch} errors={failed} total={total}")


if __name__ == "__main__":
    main()
