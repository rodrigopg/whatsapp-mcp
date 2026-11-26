import os
from pathlib import Path
import subprocess
import tempfile
import unittest

from test_install_store_dir_review import section


@unittest.skipIf(os.name == "nt", "POSIX installer")
class InstallStoreDirResetTest(unittest.TestCase):
    def test_reset_to_default_requires_moving_existing_external_store(self):
        script = ('set -euo pipefail\nsuccess() { :; }; warn() { echo "$*"; }\n'
                  + section("# ── store migration notice", "# ── build Go bridge")
                  + section('LAUNCH_SCRIPT="$INSTALL_DIR/start-bridge.sh"', "# ── service helpers"))
        for destination in ("absent", "empty"):
            with self.subTest(destination=destination), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                bridge = root / "whatsapp-bridge"
                bridge.mkdir()
                old = root / "external store"
                old.mkdir()
                (old / "whatsapp.db").write_bytes(b"session")
                (old / "messages.db").write_bytes(b"history")
                target = bridge / "store"
                if destination == "empty":
                    target.mkdir()
                env = dict(os.environ, INSTALL_DIR=tmp, BRIDGE_PORT="8080",
                           WHATSAPP_STORE_DIR=str(old))
                subprocess.run(["bash", "-c", script], env=env, cwd=root,
                               check=True, capture_output=True, text=True)
                launcher = root / "start-bridge.sh"
                before = launcher.read_bytes()

                env["WHATSAPP_STORE_DIR"] = ""
                result = subprocess.run(["bash", "-c", script], env=env, cwd=root,
                                        check=True, capture_output=True, text=True)
                self.assertEqual(launcher.read_bytes(), before)
                self.assertIn("Stop", result.stdout)
                self.assertIn("back up", result.stdout)
                self.assertIn("launch script left unchanged", result.stdout)
                self.assertEqual((old / "whatsapp.db").read_bytes(), b"session")
                self.assertEqual((old / "messages.db").read_bytes(), b"history")
                self.assertEqual(target.exists(), destination == "empty")
                if target.exists():
                    self.assertEqual(list(target.iterdir()), [])

                commands = [line for line in result.stdout.splitlines()
                            if line.startswith(("  mkdir ", "  rmdir ", "  mv "))]
                self.assertEqual(len(commands), 3 if destination == "empty" else 2)
                subprocess.run(["bash", "-ec", "\n".join(commands)], check=True)
                subprocess.run(["bash", "-c", script], env=env, cwd=root,
                               check=True, capture_output=True, text=True)
                self.assertIn("WHATSAPP_STORE_DIR='' WHATSAPP_BRIDGE_PORT=8080 exec ./whatsapp-bridge",
                              launcher.read_text())
                self.assertEqual((target / "whatsapp.db").read_bytes(), b"session")
                self.assertEqual((target / "messages.db").read_bytes(), b"history")
                self.assertFalse(old.exists())
