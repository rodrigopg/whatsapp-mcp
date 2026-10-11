import json
import os
import shutil
import stat
import subprocess
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from unittest.mock import patch

import tts
import whatsapp

FAKE_TTS_SCRIPT = '''\
import os, struct, sys, wave, math

args = sys.argv[1:]
out = args[args.index("--output_file") + 1]
text = sys.stdin.buffer.read().decode("utf-8")
record = os.environ.get("FAKE_TTS_RECORD")
if record:
    with open(record, "w", encoding="utf-8") as f:
        f.write(text)
if os.environ.get("FAKE_TTS_FAIL"):
    sys.stderr.write("voice model exploded")
    sys.exit(3)
with wave.open(out, "wb") as w:
    w.setnchannels(1)
    w.setsampwidth(2)
    w.setframerate(22050)
    w.writeframes(b"".join(struct.pack("<h", int(8000 * math.sin(i / 20.0))) for i in range(11025)))
marker = os.environ.get("FAKE_TTS_MARKER")
if marker:
    open(marker, "w").close()
'''


def make_fake_cli(directory):
    """A piper-shaped CLI: text on stdin, WAV written to --output_file."""
    script = os.path.join(directory, "fake_tts.py")
    with open(script, "w", encoding="utf-8") as f:
        f.write(FAKE_TTS_SCRIPT)
    if os.name == "nt":
        cli = os.path.join(directory, "fake_tts.cmd")
        with open(cli, "w") as f:
            f.write(f'@"{sys.executable}" "{script}" %*\r\n')
    else:
        cli = os.path.join(directory, "fake_tts")
        with open(cli, "w") as f:
            f.write(f'#!{sys.executable}\n' + FAKE_TTS_SCRIPT)
        os.chmod(cli, os.stat(cli).st_mode | stat.S_IEXEC)
    model = os.path.join(directory, "voice.onnx")
    with open(model, "wb") as f:
        f.write(b"model")
    return cli, model


def make_wav_bytes():
    import io
    import math
    import struct
    import wave
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(22050)
        w.writeframes(b"".join(struct.pack("<h", int(8000 * math.sin(i / 20.0))) for i in range(11025)))
    return buf.getvalue()


class FakeServer:
    """HTTP server on 127.0.0.1 recording requests; `respond` picks the reply."""

    def __init__(self, respond):
        self.requests = []
        outer = self

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                length = int(self.headers.get("Content-Length", 0))
                body = self.rfile.read(length)
                outer.requests.append({
                    "path": self.path,
                    "headers": {k.lower(): v for k, v in self.headers.items()},
                    "body": body,
                })
                status, ctype, payload = respond(outer.requests[-1])
                self.send_response(status)
                self.send_header("Content-Type", ctype)
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

            def log_message(self, *args):
                pass

        self.httpd = HTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.httpd.server_address[1]}"
        self.thread = threading.Thread(target=self.httpd.serve_forever, daemon=True)

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *exc):
        self.httpd.shutdown()
        self.httpd.server_close()


needs_ffmpeg = unittest.skipUnless(
    shutil.which("ffmpeg") and shutil.which("ffprobe"),
    "ffmpeg and ffprobe are required to convert and inspect audio; not found on PATH")


def codec_name(path):
    out = subprocess.run(
        ["ffprobe", "-v", "error", "-select_streams", "a:0",
         "-show_entries", "stream=codec_name", "-of", "default=nw=1:nk=1", path],
        capture_output=True, text=True, check=True)
    return out.stdout.strip()


class TTSTestCase(unittest.TestCase):
    """Isolates temp files in a dedicated dir so leftovers are detectable."""

    def setUp(self):
        self.work = tempfile.mkdtemp(prefix="tts_test_work_")
        self.scratch = tempfile.mkdtemp(prefix="tts_test_tmp_")
        self._old_tempdir = tempfile.tempdir
        tempfile.tempdir = self.scratch
        self.addCleanup(self._cleanup)
        self.cli, self.model = make_fake_cli(self.work)

    def _cleanup(self):
        tempfile.tempdir = self._old_tempdir
        shutil.rmtree(self.work, ignore_errors=True)
        shutil.rmtree(self.scratch, ignore_errors=True)

    def local(self, **extra):
        return patch.multiple(tts, TTS_ENGINE="local", TTS_CLI=self.cli, TTS_MODEL=self.model, **extra)

    def api(self, base, **extra):
        values = dict(TTS_ENGINE="api", TTS_API_KEY="sk-test-123", TTS_API_BASE=base)
        values.update(extra)
        return patch.multiple(tts, **values)

    def assertNoLeftovers(self):
        self.assertEqual(os.listdir(self.scratch), [])


class TestLocalEngine(TTSTestCase):
    @needs_ffmpeg
    def test_local_engine_produces_opus_ogg(self):
        record = os.path.join(self.work, "stdin.txt")
        with self.local(), patch.dict(os.environ, {"FAKE_TTS_RECORD": record}):
            ok, reason = tts.engine_ready()
            self.assertTrue(ok, reason)
            path = tts.synthesize("Olá, teste")
        self.assertTrue(path.endswith(".ogg"))
        self.assertTrue(os.path.isfile(path))
        self.assertEqual(codec_name(path), "opus")
        with open(record, encoding="utf-8") as f:
            self.assertEqual(f.read(), "Olá, teste")
        os.remove(path)
        self.assertNoLeftovers()

    def test_local_cli_failure_reports_reason_and_leaves_nothing(self):
        with self.local(), patch.dict(os.environ, {"FAKE_TTS_FAIL": "1"}):
            with self.assertRaises(tts.TTSError) as cm:
                tts.synthesize("Olá")
        self.assertIn("code 3", str(cm.exception))
        self.assertIn("voice model exploded", str(cm.exception))
        self.assertNoLeftovers()

    def test_local_missing_ffmpeg_is_clear_error_and_leaves_nothing(self):
        with self.local(), patch.dict(os.environ, {"PATH": self.work}):
            self.assertFalse(tts.engine_ready()[0])
            with self.assertRaises(tts.TTSError) as cm:
                tts.synthesize("Olá")
        self.assertIn("ffmpeg", str(cm.exception))
        self.assertNoLeftovers()

    def test_local_missing_cli_or_model_is_not_configured(self):
        with patch.multiple(tts, TTS_ENGINE="local", TTS_CLI="", TTS_MODEL=self.model):
            ok, reason = tts.engine_ready()
        self.assertFalse(ok)
        self.assertTrue(reason.startswith("TTS not configured"), reason)
        with patch.multiple(tts, TTS_ENGINE="local", TTS_CLI=self.cli, TTS_MODEL=""):
            ok, reason = tts.engine_ready()
        self.assertFalse(ok)
        self.assertTrue(reason.startswith("TTS not configured"), reason)


class TestApiEngine(TTSTestCase):
    @needs_ffmpeg
    def test_api_engine_posts_speech_request_and_produces_opus_ogg(self):
        wav = make_wav_bytes()
        with FakeServer(lambda req: (200, "audio/wav", wav)) as server:
            with self.api(server.url, TTS_API_MODEL="gpt-4o-mini-tts", TTS_API_VOICE="alloy"):
                ok, reason = tts.engine_ready()
                self.assertTrue(ok, reason)
                path = tts.synthesize("Olá, teste")
        self.assertEqual(len(server.requests), 1)
        req = server.requests[0]
        self.assertEqual(req["path"], "/audio/speech")
        body = json.loads(req["body"])
        self.assertEqual(body["model"], "gpt-4o-mini-tts")
        self.assertEqual(body["voice"], "alloy")
        self.assertEqual(body["input"], "Olá, teste")
        self.assertIn("sk-test-123", req["headers"]["authorization"])
        self.assertEqual(codec_name(path), "opus")
        os.remove(path)
        self.assertNoLeftovers()

    def test_api_http_errors_report_status_and_leave_nothing(self):
        for status in (401, 500):
            with self.subTest(status=status):
                with FakeServer(lambda req, s=status: (s, "application/json", b'{"error":"nope"}')) as server:
                    with self.api(server.url):
                        with self.assertRaises(tts.TTSError) as cm:
                            tts.synthesize("Olá")
                self.assertIn(str(status), str(cm.exception))
                self.assertNoLeftovers()

    def test_api_without_key_is_not_configured(self):
        with patch.multiple(tts, TTS_ENGINE="api", TTS_API_KEY=""):
            ok, reason = tts.engine_ready()
        self.assertFalse(ok)
        self.assertTrue(reason.startswith("TTS not configured"), reason)

    def test_api_defaults(self):
        env = {k: v for k, v in os.environ.items() if not k.startswith("TTS_")}
        out = subprocess.run(
            [sys.executable, "-c",
             "import tts; print(tts.TTS_API_BASE, tts.TTS_API_MODEL, tts.TTS_API_VOICE, repr(tts.TTS_ENGINE))"],
            capture_output=True, text=True, env=env, cwd=os.path.dirname(os.path.abspath(__file__)), check=True)
        self.assertEqual(out.stdout.split(),
                         ["https://api.openai.com/v1", "gpt-4o-mini-tts", "alloy", "''"])


class TestNotConfiguredAndLimits(TTSTestCase):
    def test_unset_engine_is_not_configured(self):
        with patch.multiple(tts, TTS_ENGINE=""):
            ok, reason = tts.engine_ready()
        self.assertFalse(ok)
        self.assertIn("not configured", reason)

    def test_unknown_engine_is_not_configured(self):
        with patch.multiple(tts, TTS_ENGINE="festival"):
            ok, reason = tts.engine_ready()
        self.assertFalse(ok)
        self.assertTrue(reason.startswith("TTS not configured"), reason)

    def test_rejects_text_over_limit(self):
        record = os.path.join(self.work, "stdin.txt")
        with self.local(), patch.dict(os.environ, {"FAKE_TTS_RECORD": record}):
            with self.assertRaises(tts.TTSError) as cm:
                tts.synthesize("a" * 4097)
        self.assertIn("4096", str(cm.exception))
        self.assertFalse(os.path.exists(record), "the engine must not be called")
        self.assertNoLeftovers()

    @needs_ffmpeg
    def test_accepts_text_at_limit(self):
        with self.local():
            path = tts.synthesize("a" * 4096)
        self.assertEqual(codec_name(path), "opus")
        os.remove(path)


class TestSendVoiceMessage(TTSTestCase):
    # Same placeholder JID the tool docstrings use; never a phone-shaped number.
    RECIPIENT = "123456789@s.whatsapp.net"

    def bridge(self):
        """Fake bridge: records each request and whether its media file existed."""
        seen = []

        def respond(req):
            payload = json.loads(req["body"])
            media = payload.get("media_path")
            seen.append({"path": req["path"], "payload": payload,
                         "existed": bool(media) and os.path.isfile(media)})
            return 200, "application/json", json.dumps({"success": True, "message": "Message sent"}).encode()

        server = FakeServer(respond)
        server.seen = seen
        return server

    def call(self, server, text):
        with patch.object(whatsapp, "WHATSAPP_API_BASE_URL", server.url + "/api"):
            return whatsapp.send_voice_message(self.RECIPIENT, text)

    def test_send_voice_not_configured_sends_nothing(self):
        with self.bridge() as server, patch.multiple(tts, TTS_ENGINE=""):
            success, message = self.call(server, "Olá")
        self.assertFalse(success)
        self.assertIn("not configured", message)
        self.assertEqual(server.requests, [])
        self.assertNoLeftovers()

    @needs_ffmpeg
    def test_send_voice_sends_one_existing_ogg_then_deletes_it(self):
        with self.bridge() as server, self.local():
            success, message = self.call(server, "Olá")
        self.assertTrue(success, message)
        self.assertEqual(len(server.seen), 1)
        sent = server.seen[0]
        self.assertEqual(sent["path"], "/api/send")
        self.assertEqual(sent["payload"]["recipient"], self.RECIPIENT)
        self.assertTrue(sent["payload"]["media_path"].endswith(".ogg"))
        self.assertTrue(sent["existed"], "the .ogg must exist when the bridge is called")
        self.assertFalse(os.path.exists(sent["payload"]["media_path"]))
        self.assertNoLeftovers()

    @needs_ffmpeg
    def test_send_voice_text_over_limit_sends_nothing(self):
        with self.bridge() as server, self.local():
            success, message = self.call(server, "a" * 4097)
        self.assertFalse(success)
        self.assertIn("4096", message)
        self.assertEqual(server.requests, [])
        self.assertNoLeftovers()

    @needs_ffmpeg
    def test_send_voice_engine_failure_sends_nothing(self):
        with self.bridge() as server, self.local(), patch.dict(os.environ, {"FAKE_TTS_FAIL": "1"}):
            success, message = self.call(server, "Olá")
        self.assertFalse(success)
        self.assertIn("voice model exploded", message)
        self.assertEqual(server.requests, [])
        self.assertNoLeftovers()

    @needs_ffmpeg
    def test_send_voice_deletes_temp_even_when_bridge_rejects(self):
        server = FakeServer(lambda req: (500, "text/plain", b"boom"))
        with server, self.local():
            success, message = self.call(server, "Olá")
        self.assertFalse(success)
        self.assertEqual(len(server.requests), 1)
        self.assertNoLeftovers()

    def test_send_voice_requires_recipient_and_text(self):
        with self.bridge() as server, self.local():
            with patch.object(whatsapp, "WHATSAPP_API_BASE_URL", server.url + "/api"):
                self.assertFalse(whatsapp.send_voice_message("", "Olá")[0])
                self.assertFalse(whatsapp.send_voice_message(self.RECIPIENT, "   ")[0])
        self.assertEqual(server.requests, [])


class TestRobustness(TTSTestCase):
    """Failures that must come back as (False, reason), never as an exception."""

    RECIPIENT = TestSendVoiceMessage.RECIPIENT
    bridge = TestSendVoiceMessage.bridge
    call = TestSendVoiceMessage.call

    def test_api_auth_error_does_not_echo_body(self):
        api = FakeServer(lambda req: (401, "application/json",
                                      b'{"error": {"message": "Incorrect API key provided: sk-...abcd"}}'))
        with self.bridge() as server, api, self.api(api.url),                 patch.object(tts.shutil, "which", return_value="ffmpeg"):
            success, message = self.call(server, "Olá")
        self.assertFalse(success)
        self.assertIn("401", message)
        self.assertIn("TTS_API_KEY", message)
        self.assertNotIn("sk-", message)
        self.assertEqual(server.requests, [])
        self.assertNoLeftovers()

    def test_lone_surrogate_is_refused_without_exception(self):
        for engine in (self.local, lambda: self.api("http://127.0.0.1:9")):
            with self.subTest(engine=engine), self.bridge() as server, engine(), \
                    patch.object(tts.shutil, "which", return_value="ffmpeg"):
                success, message = self.call(server, "oi \ud83d")
            self.assertFalse(success)
            self.assertIn("encoded", message)
            self.assertEqual(server.requests, [])
            self.assertNoLeftovers()


class TestNotice(TTSTestCase):
    RECIPIENT = TestSendVoiceMessage.RECIPIENT
    bridge = TestSendVoiceMessage.bridge

    def call(self, server, text, notice):
        with patch.object(whatsapp, "WHATSAPP_API_BASE_URL", server.url + "/api"):
            return whatsapp.send_voice_message(self.RECIPIENT, text, notice)

    @needs_ffmpeg
    def test_notice_is_sent_after_synthesis_right_before_audio(self):
        marker = os.path.join(self.work, "synthesis.done")
        seen = []

        def respond(req):
            payload = json.loads(req["body"])
            seen.append({"payload": payload, "marker_existed": os.path.exists(marker)})
            return 200, "application/json", json.dumps({"success": True, "message": "Message sent"}).encode()

        with FakeServer(respond) as server, self.local(), patch.dict(os.environ, {"FAKE_TTS_MARKER": marker}):
            success, message = self.call(server, "Olá", "aviso")
        self.assertTrue(success, message)
        self.assertEqual([r["path"] for r in server.requests], ["/api/send", "/api/send"])
        first, second = seen
        self.assertEqual(first["payload"]["message"], "aviso")
        self.assertNotIn("media_path", first["payload"])
        self.assertTrue(second["payload"]["media_path"].endswith(".ogg"))
        self.assertTrue(first["marker_existed"], "the speech must be generated before the notice is sent")
        self.assertNoLeftovers()

    @needs_ffmpeg
    def test_notice_with_engine_failure_sends_nothing(self):
        with self.bridge() as server, self.local(), patch.dict(os.environ, {"FAKE_TTS_FAIL": "1"}):
            success, message = self.call(server, "Olá", "aviso")
        self.assertFalse(success)
        self.assertEqual(server.requests, [])
        self.assertNoLeftovers()

    @needs_ffmpeg
    def test_notice_rejected_by_bridge_skips_audio(self):
        def respond(req):
            payload = json.loads(req["body"])
            if payload.get("media_path"):
                return 200, "application/json", json.dumps({"success": True, "message": "Message sent"}).encode()
            return 200, "application/json", json.dumps({"success": False, "message": "notice refused"}).encode()

        with FakeServer(respond) as server, self.local():
            success, message = self.call(server, "Olá", "aviso")
        self.assertFalse(success)
        self.assertIn("notice refused", message)
        self.assertEqual(len(server.requests), 1)
        self.assertNotIn("media_path", json.loads(server.requests[0]["body"]))
        self.assertNoLeftovers()

    @needs_ffmpeg
    def test_no_notice_sends_only_the_audio(self):
        with self.bridge() as server, self.local():
            success, message = self.call(server, "Olá", "")
        self.assertTrue(success, message)
        self.assertEqual(len(server.seen), 1)
        self.assertTrue(server.seen[0]["payload"]["media_path"].endswith(".ogg"))
        self.assertNotIn("message", server.seen[0]["payload"])
        self.assertNoLeftovers()


if __name__ == "__main__":
    unittest.main()
