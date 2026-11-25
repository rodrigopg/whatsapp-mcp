import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


class StoreDirReviewTest(unittest.TestCase):
    def test_relative_store_is_rejected_from_any_cwd(self):
        server = Path(__file__).resolve().parent
        with tempfile.TemporaryDirectory() as tmp:
            for cwd in (server.parent, server.parent / "whatsapp-bridge", Path(tmp)):
                for value in ("relative data", "~/literal", "../outside"):
                    with self.subTest(cwd=cwd, value=value):
                        result = subprocess.run(
                            [sys.executable, "-c", "import store_paths"], cwd=cwd,
                            env=dict(os.environ, PYTHONPATH=str(server), WHATSAPP_STORE_DIR=value),
                            capture_output=True, text=True,
                        )
                        self.assertNotEqual(result.returncode, 0)
                        self.assertIn("WHATSAPP_STORE_DIR must be an absolute path", result.stderr)
