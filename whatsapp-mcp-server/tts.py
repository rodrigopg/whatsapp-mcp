"""Text-to-speech for send_voice_message.

Two opt-in engines, selected with TTS_ENGINE (unset = not configured, nothing is
generated or sent):
  - "local": an external CLI (e.g. piper) that reads the text on stdin and writes
             a WAV to --output_file. No Python dependency is added; the CLI and the
             voice model are pointed at by path (TTS_CLI, TTS_MODEL).
  - "api":   any OpenAI-compatible /audio/speech endpoint (TTS_API_KEY, TTS_API_BASE,
             TTS_API_MODEL, TTS_API_VOICE).

Either engine's output is converted to Opus in an Ogg container (ffmpeg) so the
bridge can send it as a WhatsApp voice message.
"""

import logging
import os
import shutil
import subprocess
import tempfile
from urllib.parse import urlparse

import requests

import audio

log = logging.getLogger(__name__)

# Engine selection: "local" | "api". Empty means not configured.
TTS_ENGINE = os.environ.get("TTS_ENGINE", "").lower()

# Local backend: external CLI reading text on stdin (piper CLI shape).
TTS_CLI = os.environ.get("TTS_CLI", "")
TTS_MODEL = os.environ.get("TTS_MODEL", "")

# API backend: OpenAI-compatible /audio/speech.
TTS_API_KEY = os.environ.get("TTS_API_KEY", "")
TTS_API_BASE = os.environ.get("TTS_API_BASE", "https://api.openai.com/v1")
TTS_API_MODEL = os.environ.get("TTS_API_MODEL", "gpt-4o-mini-tts")
TTS_API_VOICE = os.environ.get("TTS_API_VOICE", "alloy")

# OpenAI's input limit; longer text is refused, never truncated.
MAX_TTS_CHARS = 4096
TTS_TIMEOUT = 120


class TTSError(Exception):
    """Synthesis failed. The message reaches MCP clients (possibly remote ones), so it is a
    generic reason plus a status/exit code only: never upstream bodies, stderr, paths or
    exception text. Detail goes to the server-side log."""


def engine_ready():
    """Return (ok, reason). Every not-ready reason starts with "TTS not configured"."""
    if TTS_ENGINE == "local":
        if not TTS_CLI or not os.path.exists(TTS_CLI):
            return False, f"TTS not configured: local engine CLI not found (set TTS_CLI)"
        if not TTS_MODEL or not os.path.exists(TTS_MODEL):
            return False, f"TTS not configured: local engine model not found (set TTS_MODEL)"
        if not shutil.which("ffmpeg"):
            return False, "TTS not configured: ffmpeg not found"
        return True, "local"
    if TTS_ENGINE == "api":
        if not TTS_API_KEY:
            return False, "TTS not configured: api engine needs TTS_API_KEY"
        base = urlparse(TTS_API_BASE)
        if base.scheme != "https" and not (base.scheme == "http" and base.hostname in ("localhost", "127.0.0.1", "::1")):
            return False, "TTS not configured: TTS_API_BASE must be https (http is only allowed for localhost)"
        if not shutil.which("ffmpeg"):
            return False, "TTS not configured: ffmpeg not found"
        return True, "api"
    if not TTS_ENGINE:
        return False, "TTS not configured: set TTS_ENGINE to 'local' or 'api'"
    return False, f"TTS not configured: unknown TTS_ENGINE (use 'local' or 'api')"


def synthesize(text):
    """Turn text into speech and return the path of a temporary .ogg (Opus).

    The caller owns the file and must delete it. Raises TTSError on any failure,
    leaving no temporary files behind. Callers check engine_ready() first.
    """
    if len(text) > MAX_TTS_CHARS:
        raise TTSError(f"text is {len(text)} characters; the limit is {MAX_TTS_CHARS}")

    try:
        text.encode("utf-8")
    except UnicodeEncodeError:
        raise TTSError("text has characters that cannot be encoded (lone surrogate?)")

    tmpdir = tempfile.mkdtemp(prefix="wa_tts_")
    try:
        if TTS_ENGINE == "api":
            raw = _synthesize_api(text, tmpdir)
        else:
            raw = _synthesize_local(text, tmpdir)
        try:
            return audio.convert_to_opus_ogg_temp(raw)
        except Exception as e:
            log.error("TTS audio conversion failed: %s", e)
            raise TTSError("audio conversion failed (is ffmpeg installed?)")
    finally:
        shutil.rmtree(tmpdir, ignore_errors=True)


def _synthesize_local(text, tmpdir):
    wav = os.path.join(tmpdir, "speech.wav")
    try:
        proc = subprocess.run(
            [TTS_CLI, "--model", TTS_MODEL, "--output_file", wav],
            input=text.encode("utf-8"), capture_output=True, timeout=TTS_TIMEOUT,
        )
    except (OSError, subprocess.TimeoutExpired) as e:
        log.error("TTS CLI could not run: %s", e)
        raise TTSError("TTS CLI could not run (failed to start or timed out)")
    if proc.returncode != 0:
        stderr = proc.stderr.decode("utf-8", "ignore")[-300:].strip()
        log.error("TTS CLI exited with code %s: %s", proc.returncode, stderr)
        raise TTSError(f"TTS CLI failed (exit code {proc.returncode})")
    if not os.path.isfile(wav) or os.path.getsize(wav) == 0:
        raise TTSError("TTS CLI produced no audio")
    return wav


def _synthesize_api(text, tmpdir):
    try:
        r = requests.post(
            f"{TTS_API_BASE}/audio/speech",
            headers={"Authorization": f"Bearer {TTS_API_KEY}"},
            json={"model": TTS_API_MODEL, "voice": TTS_API_VOICE, "input": text,
                  "response_format": "wav"},
            timeout=TTS_TIMEOUT,
        )
    except requests.RequestException as e:
        log.error("TTS API request failed: %s", e)
        raise TTSError("TTS API request failed (network error or timeout)")
    if r.status_code in (401, 403):
        # The body can echo the (masked) key, so it is deliberately left out.
        raise TTSError(f"TTS API rejected the credentials (HTTP {r.status_code}); check TTS_API_KEY")
    if r.status_code != 200:
        log.error("TTS API returned HTTP %s: %s", r.status_code, r.text[:200])
        raise TTSError(f"TTS API returned an error (HTTP {r.status_code})")
    if not r.content:
        raise TTSError("TTS API returned no audio")
    wav = os.path.join(tmpdir, "speech.wav")
    with open(wav, "wb") as f:
        f.write(r.content)
    return wav
