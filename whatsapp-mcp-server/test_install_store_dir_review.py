import os
from pathlib import Path
import subprocess
import tempfile
import unittest


INSTALLER = (Path(__file__).resolve().parent.parent / "install.sh").read_text()


def section(start, end):
    return INSTALLER[INSTALLER.index(start):INSTALLER.index(end, INSTALLER.index(start))]


@unittest.skipIf(os.name == "nt", "POSIX installer")
class InstallStoreDirReviewTest(unittest.TestCase):
    def run_install(self, root, value=None):
        env = dict(os.environ, INSTALL_DIR=str(root), BRIDGE_PORT="8080")
        env.pop("WHATSAPP_STORE_DIR", None)
        if value is not None:
            env["WHATSAPP_STORE_DIR"] = value
        script = ('set -euo pipefail\nsuccess() { :; }; warn() { echo "$*"; }\n'
                  + 'STORE_DIR="${WHATSAPP_STORE_DIR:-}"\n'
                  + section("# ── store migration notice", "# ── build Go bridge")
                  + section('LAUNCH_SCRIPT="$INSTALL_DIR/start-bridge.sh"', "# ── service helpers"))
        return subprocess.run(["bash", "-c", script], env=env, cwd=root,
                              check=True, capture_output=True, text=True)

    def fixture(self, root):
        bridge = root / "whatsapp-bridge"
        bridge.mkdir()
        stub = bridge / "whatsapp-bridge"
        stub.write_text('#!/bin/sh\nprintf "%s" "$WHATSAPP_STORE_DIR" > probe.txt\n')
        stub.chmod(0o700)
        return bridge

    def launch_store(self, root):
        env = dict(os.environ)
        env.pop("WHATSAPP_STORE_DIR", None)
        subprocess.run([str(root / "start-bridge.sh")], env=env, check=True, capture_output=True)
        return (root / "whatsapp-bridge/probe.txt").read_text()

    def test_reinstall_preserves_saved_store_but_empty_resets_it(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            self.fixture(root)
            value = str(root / "store ' $HOME `false` $(false)\nline\n")
            self.run_install(root, value)
            self.run_install(root)
            self.assertEqual(self.launch_store(root), value)
            self.run_install(root, "")
            self.assertEqual(self.launch_store(root), "")

    def test_relative_install_path_is_saved_as_absolute(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            bridge = self.fixture(root)
            self.run_install(root, "relative data")
            self.assertEqual(self.launch_store(root), str(bridge / "relative data"))

    def test_pending_migration_preserves_launcher_until_move(self):
        for external in (False, True):
            with self.subTest(external=external), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                bridge = self.fixture(root)
                old = root / "previous" if external else bridge / "store"
                old.mkdir()
                (old / "whatsapp.db").write_bytes(b"session")
                self.run_install(root, str(old) if external else "")
                launcher = root / "start-bridge.sh"
                before = launcher.read_bytes()
                target = root / "external"
                result = self.run_install(root, str(target))
                self.assertIn("back up", result.stdout)
                self.assertEqual(launcher.read_bytes(), before)
                self.assertEqual(self.launch_store(root), str(old) if external else "")
                self.assertFalse(target.exists())
                old.rename(target)
                self.run_install(root, str(target))
                self.assertEqual(self.launch_store(root), str(target))
                self.assertEqual((target / "whatsapp.db").read_bytes(), b"session")

    def test_pending_migration_does_not_create_launcher(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            bridge = self.fixture(root)
            old = bridge / "store"
            old.mkdir()
            (old / "whatsapp.db").write_bytes(b"session")
            self.run_install(root, str(root / "external"))
            self.assertFalse((root / "start-bridge.sh").exists())
            self.assertEqual((old / "whatsapp.db").read_bytes(), b"session")
