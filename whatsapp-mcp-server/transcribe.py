"""Backfill audio transcriptions into messages.content.

Headless transcription pipeline reusing the local whisper.cpp build. For each
audio message with empty content, it downloads the media through the bridge,
verifies the bytes against the stored plaintext SHA-256 (the stored filename is
derived from sync time and collides, so the SHA is the only reliable identity
guard), converts to 16 kHz mono WAV, runs whisper-cli, and writes the result
back into messages.content so the normal accent-insensitive search finds it.

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
import os
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import time
from datetime import datetime, timedelta, timezone

import requests

# WhatsApp purges undelivered media from its CDN after roughly 2-3 weeks. Past
# this age a download failure is permanent; before it, treat failures as transient.
CDN_EXPIRY = timedelta(days=21)

DB_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', 'whatsapp-bridge', 'store', 'messages.db')
API_BASE = os.environ.get("WHATSAPP_API_BASE_URL", "http://localhost:8080/api")

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
    """Download via the bridge. Returns local path, or None on failure."""
    r = requests.post(f"{API_BASE}/download",
                      json={"message_id": message_id, "chat_jid": chat_jid},
                      timeout=120)
    if r.status_code == 200 and r.json().get("success"):
        return r.json().get("path")
    return None


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


def _transcribe_api(ogg_path):
    """OpenAI-compatible STT (/audio/transcriptions). Serves OpenAI or Groq via env."""
    size = os.path.getsize(ogg_path)
    if size > API_MAX_BYTES:
        # Don't silently drop — voice notes are tiny, so this is rare; surface it.
        raise RuntimeError(f"audio {size} bytes exceeds API limit {API_MAX_BYTES}")
    with open(ogg_path, "rb") as f:
        r = requests.post(
            f"{TRANSCRIPTION_API_BASE}/audio/transcriptions",
            headers={"Authorization": f"Bearer {TRANSCRIPTION_API_KEY}"},
            files={"file": (os.path.basename(ogg_path), f, "audio/ogg")},
            data={"model": TRANSCRIPTION_API_MODEL, "language": "pt", "prompt": WHISPER_PROMPT},
            timeout=120,
        )
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
            # Bust any stale cached file (filename collisions) before downloading.
            chat_dir = os.path.join(os.path.dirname(DB_PATH), chat_jid.replace(":", "_"))
            if os.path.isdir(chat_dir):
                for fn in os.listdir(chat_dir):
                    if fn.startswith(msg_id + "_"):
                        try:
                            os.remove(os.path.join(chat_dir, fn))
                        except OSError:
                            pass

            path = download(msg_id, chat_jid)
            if not path or not os.path.isfile(path):
                # Only mark permanently unavailable once the CDN window has
                # certainly passed. A recent audio that fails is likely a
                # transient blip — leave content='' so the next sweep retries
                # instead of silently losing a live message.
                if _is_expired(ts):
                    write_content(msg_id, chat_jid, SENTINEL_UNAVAILABLE)
                    unavailable += 1
                    log(f"{prefix} unavailable (expired CDN, download failed)")
                else:
                    failed += 1
                    log(f"{prefix} download failed but recent — will retry next sweep")
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
        except Exception as e:
            failed += 1
            log(f"{prefix} ERROR: {e}")

    log(f"DONE. transcribed={done} empty={empty} unavailable={unavailable} "
        f"sha_mismatch={mismatch} errors={failed} total={total}")


if __name__ == "__main__":
    main()
