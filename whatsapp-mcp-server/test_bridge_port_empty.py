import os
import subprocess
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))


def _base_url(module, attr):
    env = {k: v for k, v in os.environ.items() if k != "WHATSAPP_API_BASE_URL"}
    env["WHATSAPP_BRIDGE_PORT"] = ""
    out = subprocess.run(
        [sys.executable, "-c", f"import {module}; print({module}.{attr})"],
        cwd=HERE, env=env, capture_output=True, text=True, check=True,
    )
    return out.stdout.strip()


class EmptyBridgePortTest(unittest.TestCase):
    """The Go bridge treats an empty WHATSAPP_BRIDGE_PORT as 8080; the Python side must agree."""

    def test_mcp_server(self):
        self.assertEqual(_base_url("whatsapp", "WHATSAPP_API_BASE_URL"), "http://localhost:8080/api")

    def test_transcribe(self):
        self.assertEqual(_base_url("transcribe", "API_BASE"), "http://localhost:8080/api")


if __name__ == "__main__":
    unittest.main()
