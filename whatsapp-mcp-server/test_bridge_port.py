import os
import subprocess
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))


def _base_url(**env):
    clean = {k: v for k, v in os.environ.items() if k not in ("WHATSAPP_API_BASE_URL", "WHATSAPP_BRIDGE_PORT")}
    out = subprocess.run(
        [sys.executable, "-c", "import whatsapp; print(whatsapp.WHATSAPP_API_BASE_URL)"],
        cwd=HERE, env={**clean, **env}, capture_output=True, text=True, check=True,
    )
    return out.stdout.strip()


class BridgePortTest(unittest.TestCase):
    def test_default_port(self):
        self.assertEqual(_base_url(), "http://localhost:8080/api")

    def test_bridge_port_from_installer(self):
        self.assertEqual(_base_url(WHATSAPP_BRIDGE_PORT="18081"), "http://localhost:18081/api")

    def test_explicit_url_wins(self):
        self.assertEqual(
            _base_url(WHATSAPP_BRIDGE_PORT="18081", WHATSAPP_API_BASE_URL="http://10.0.0.5:9000/api"),
            "http://10.0.0.5:9000/api",
        )


if __name__ == "__main__":
    unittest.main()
