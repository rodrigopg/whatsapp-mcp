import os
from pathlib import Path
import plistlib
import subprocess
import tempfile
import unittest


@unittest.skipIf(os.name == "nt", "POSIX installer")
class InstallStoreDirTest(unittest.TestCase):
    def test_launcher_persists_store_dir_without_shell_expansion(self):
        installer = (Path(__file__).resolve().parent.parent / "install.sh").read_text()
        start = installer.index('LAUNCH_SCRIPT="$INSTALL_DIR/start-bridge.sh"')
        end = installer.index("# ── service helpers", start)
        for value in ("", "relative data", "~/literal", "/tmp/store ' $HOME `false` $(false)"):
            with self.subTest(value=value), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                bridge = root / "whatsapp-bridge"
                bridge.mkdir()
                stub = bridge / "whatsapp-bridge"
                stub.write_text('#!/bin/sh\nprintf "%s" "$WHATSAPP_STORE_DIR" > probe.txt\n')
                stub.chmod(0o700)
                env = dict(os.environ, INSTALL_DIR=tmp, BRIDGE_PORT="8080", STORE_DIR=value)
                subprocess.run(["bash", "-c", "success() { :; }\n" + installer[start:end]],
                               env=env, check=True, capture_output=True)
                env.pop("WHATSAPP_STORE_DIR", None)
                subprocess.run([str(root / "start-bridge.sh")], env=env, check=True, capture_output=True)
                self.assertEqual((bridge / "probe.txt").read_text(), value)

    def test_migration_notice_leaves_data_in_place(self):
        installer = (Path(__file__).resolve().parent.parent / "install.sh").read_text()
        start = installer.index("# ── store migration notice")
        end = installer.index("# ── build Go bridge", start)
        for destination in ("absent", "empty", "populated", "same"):
            with self.subTest(destination=destination), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                old = root / "whatsapp-bridge" / "store"
                old.mkdir(parents=True)
                (old / "whatsapp.db").write_bytes(b"session")
                target = old if destination == "same" else root / "external ' store"
                if destination in ("empty", "populated"):
                    target.mkdir()
                if destination == "populated":
                    (target / "messages.db").write_bytes(b"history")
                env = dict(os.environ, INSTALL_DIR=tmp, WHATSAPP_STORE_DIR=str(target))
                script = ('set -eu\nwarn() { echo "$*"; }\n' + installer[start:end]
                          + 'printf "pending=%s\\n" "$STORE_MIGRATION_PENDING"')
                result = subprocess.run(["bash", "-c", script], env=env, check=True,
                                        capture_output=True, text=True)
                pending = destination in ("absent", "empty")
                self.assertIn(f"pending={int(pending)}", result.stdout)
                self.assertEqual((old / "whatsapp.db").read_bytes(), b"session")
                self.assertEqual(target.exists(), destination != "absent")
                if pending:
                    self.assertIn("Stop", result.stdout)
                    self.assertIn("back up", result.stdout)
                    commands = [line for line in result.stdout.splitlines()
                                if line.startswith(("  mkdir ", "  rmdir ", "  mv "))]
                    subprocess.run(["bash", "-ec", "\n".join(commands)], check=True)
                    self.assertEqual((target / "whatsapp.db").read_bytes(), b"session")

    def test_services_use_launcher_and_skip_start_during_migration(self):
        installer = (Path(__file__).resolve().parent.parent / "install.sh").read_text()
        start = installer.index("# ── service helpers")
        end = installer.index('if [[ "$PLATFORM" == "macos" ]]', start)
        for pending in (0, 1):
            with self.subTest(pending=pending), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                env = dict(os.environ, HOME=tmp, INSTALL_DIR=tmp, STORE_MIGRATION_PENDING=str(pending),
                           PLIST_PATH=str(root / "bridge.plist"))
                script = (installer[start:end]
                          + '\nbridge_already_running() { return 1; }\n'
                          + 'systemctl() { :; }; loginctl() { :; }; launchctl() { [[ "$1" != print ]]; }; '
                          + 'warn() { :; }; success() { :; }; '
                          + 'install_service_linux; install_service_macos')
                subprocess.run(["bash", "-ec", script], env=env, check=True, capture_output=True)
                unit = root / ".config/systemd/user/whatsapp-bridge.service"
                plist = root / "bridge.plist"
                self.assertEqual(unit.exists(), not pending)
                self.assertEqual(plist.exists(), not pending)
                if not pending:
                    self.assertIn(f"ExecStart={tmp}/start-bridge.sh", unit.read_text())
                    self.assertEqual(plistlib.loads(plist.read_bytes())["ProgramArguments"],
                                     [str(root / "start-bridge.sh")])
