import os
import socket
import tempfile
import threading
import unittest
from unittest.mock import patch

import whatsapp


class BridgeTimeoutTests(unittest.TestCase):
    """A bridge that accepts the connection and never answers must not block a tool call forever."""

    def setUp(self):
        self.server = socket.socket()
        self.server.bind(("127.0.0.1", 0))
        self.server.listen()
        self.accepted = []
        threading.Thread(target=self._accept, daemon=True).start()
        port = self.server.getsockname()[1]
        for name, value in (("WHATSAPP_API_BASE_URL", f"http://127.0.0.1:{port}/api"),
                            ("SEND_TIMEOUT", 0.2), ("MEDIA_TIMEOUT", 0.2)):
            p = patch.object(whatsapp, name, value, create=True)
            p.start()
            self.addCleanup(p.stop)
        fd, self.media = tempfile.mkstemp(suffix=".ogg")
        os.close(fd)
        self.addCleanup(os.remove, self.media)

    def tearDown(self):
        self.server.close()
        for conn, _ in self.accepted:
            conn.close()

    def _accept(self):
        try:
            while True:
                self.accepted.append(self.server.accept())
        except OSError:
            pass

    def assertReturns(self, call):
        result = []
        t = threading.Thread(target=lambda: result.append(call()), daemon=True)
        t.start()
        t.join(5)
        self.assertTrue(result, "call still blocked on a silent bridge")
        return result[0]

    def test_sends_give_up_and_say_the_outcome_is_unknown(self):
        for name, call in (("send_message", lambda: whatsapp.send_message("123", "hi")),
                           ("send_file", lambda: whatsapp.send_file("123", self.media)),
                           ("send_audio_message", lambda: whatsapp.send_audio_message("123", self.media)),
                           ("create_group", lambda: whatsapp.create_group("g", ["123"]))):
            with self.subTest(name):
                result = self.assertReturns(call)
                self.assertFalse(result[0])
                self.assertIn("check", result[1])

    def test_download_gives_up(self):
        path, _ = self.assertReturns(lambda: whatsapp.download_media("id", "123@s.whatsapp.net"))
        self.assertIsNone(path)


if __name__ == "__main__":
    unittest.main()
