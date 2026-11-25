import os
from pathlib import Path
import shlex
import shutil
import subprocess
import sys
import tempfile
import unittest


class DockerTranscriptionTest(unittest.TestCase):
    def test_copied_transcription_scripts_can_start(self):
        repo = Path(__file__).resolve().parent.parent
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for line in (repo / "Dockerfile").read_text().splitlines():
                if not line.startswith("COPY "):
                    continue
                parts = shlex.split(line)
                if parts[:1] == ["COPY"] and parts[-1] == "/app/whatsapp-mcp-server/":
                    for source in parts[1:-1]:
                        shutil.copy(repo / source, root)
            result = subprocess.run([sys.executable, str(root / "transcribe.py"), "--help"],
                                    cwd=tmp, env={k: v for k, v in os.environ.items() if k != "PYTHONPATH"},
                                    capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("--limit", result.stdout)
