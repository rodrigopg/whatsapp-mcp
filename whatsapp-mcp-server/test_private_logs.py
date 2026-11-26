"""Transcripts stay out of the sweep's log unless WHATSAPP_LOG_CONTENT is set."""
import contextlib
import io
import os
import tempfile
import unittest
from unittest.mock import patch

import recover_audios
import transcribe

FLAGS = {"": False, "0": False, "false": False, "1": True, "true": True, "yes": True, " ON ": True}


class TranscriptLogTest(unittest.TestCase):
    def test_sweep_logs_length_and_text_only_on_request(self):
        for flag, want in FLAGS.items():
            with self.subTest(flag=flag), tempfile.NamedTemporaryFile() as audio:
                output = io.StringIO()
                with patch.dict(os.environ, {"WHATSAPP_LOG_CONTENT": flag}), \
                     patch("sys.argv", ["transcribe.py"]), \
                     patch.object(transcribe, "engine_ready", return_value=(True, "local")), \
                     patch.object(transcribe.sqlite3, "connect"), \
                     patch.object(transcribe, "DB_PATH", os.path.join(tempfile.gettempdir(), "absent", "messages.db")), \
                     patch.object(transcribe, "pending_audios", return_value=[("msg", "chat", None, None, "2026-11-25")]), \
                     patch.object(transcribe, "cached_audio", return_value=None), \
                     patch.object(transcribe, "valid_audio_file", return_value=True), \
                     patch.object(transcribe, "download", return_value=(audio.name, None, False)), \
                     patch.object(transcribe, "transcribe", return_value="PRIVATE TRANSCRIPT"), \
                     patch.object(transcribe, "write_content"), contextlib.redirect_stdout(output):
                    transcribe.main()
                self.assertEqual("PRIVATE TRANSCRIPT" in output.getvalue(), want)
                self.assertIn("18 chars", output.getvalue())

    def test_recovery_log_line(self):
        for flag, want in FLAGS.items():
            with self.subTest(flag=flag):
                output = io.StringIO()
                with patch.dict(os.environ, {"WHATSAPP_LOG_CONTENT": flag}), contextlib.redirect_stdout(output):
                    recover_audios.log("  msg ok (18 chars)", content="PRIVATE TRANSCRIPT")
                self.assertEqual("PRIVATE TRANSCRIPT" in output.getvalue(), want)


if __name__ == "__main__":
    unittest.main()
