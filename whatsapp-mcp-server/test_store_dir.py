import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


class StoreDirTest(unittest.TestCase):
    def test_clone_voice_defaults_to_configured_store(self):
        code = """
import clone_voice
from unittest.mock import patch
with patch.object(clone_voice, 'rows', return_value=[]) as rows:
    try:
        clone_voice.main()
    except SystemExit:
        pass
    print(rows.call_args.args[0])
"""
        with tempfile.TemporaryDirectory() as tmp:
            store = Path(tmp) / "external"
            env = dict(os.environ, WHATSAPP_STORE_DIR=str(store),
                       PYTHONPATH=str(Path(__file__).resolve().parent))
            result = subprocess.run([sys.executable, "-c", code], cwd=tmp, env=env,
                                    check=True, capture_output=True, text=True)
            self.assertEqual(result.stdout.strip(), str(store / "messages.db"))

    def test_python_paths_use_default_or_absolute_store(self):
        bridge = Path(__file__).resolve().parent.parent / "whatsapp-bridge"
        code = ("import json,store_paths,transcribe,recover_audios; "
                "print(json.dumps([str(store_paths.STORE_DIR),transcribe.DB_PATH,recover_audios.DB_PATH]))")
        with tempfile.TemporaryDirectory() as tmp:
            for value in (None, "", str(Path(tmp) / "external [data] #?%")):
                with self.subTest(value=value):
                    env = dict(os.environ)
                    env.pop("WHATSAPP_STORE_DIR", None)
                    if value is not None:
                        env["WHATSAPP_STORE_DIR"] = value
                    env["PYTHONPATH"] = str(Path(__file__).resolve().parent)
                    result = subprocess.run([sys.executable, "-c", code], cwd=tmp, env=env,
                                            check=True, capture_output=True, text=True)
                    root = Path(value) if value else bridge / "store"
                    self.assertEqual([Path(path).resolve() for path in json.loads(result.stdout)],
                                     [root.resolve(), (root / "messages.db").resolve(),
                                      (root / "messages.db").resolve()])
