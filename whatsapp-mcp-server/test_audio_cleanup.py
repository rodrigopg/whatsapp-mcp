import os
import tempfile
import unittest
from unittest.mock import Mock, patch

import requests

import whatsapp


class AudioCleanup(unittest.TestCase):
    def test_converted_file_removed_on_success_and_failure(self):
        cases = (
            {"return_value": Mock(status_code=200, json=Mock(return_value={"success": True}))},
            {"return_value": Mock(status_code=500, text="failed")},
            {"side_effect": requests.ReadTimeout()},
            {"return_value": Mock(status_code=200, json=Mock(side_effect=ValueError("invalid JSON")))},
        )
        for kwargs in cases:
            with self.subTest(kwargs), tempfile.TemporaryDirectory() as directory:
                source, converted = os.path.join(directory, "input.wav"), os.path.join(directory, "converted.ogg")
                with open(source, "wb") as stream:
                    stream.write(b"wave")
                with open(converted, "wb") as stream:
                    stream.write(b"OggS")
                copies = []

                def convert(path):
                    copies.append(path)
                    with open(path, "rb") as f:
                        self.assertEqual(f.read(), b"wave")
                    return converted

                def post(*args, **kw):
                    self.assertTrue(os.path.exists(converted), "audio removed before upload")
                    self.assertEqual(kw["data"], b"OggS")
                    if "side_effect" in kwargs:
                        raise kwargs["side_effect"]
                    return kwargs["return_value"]

                with patch("whatsapp.audio.convert_to_opus_ogg_temp", side_effect=convert), \
                        patch("whatsapp.requests.post", side_effect=post) as sent:
                    whatsapp.send_audio_message("test", source)
                self.assertEqual(sent.call_count, 1)
                self.assertNotEqual(copies, [source], "ffmpeg must read the checked copy, not the path")
                self.assertFalse(os.path.exists(converted))
                self.assertFalse(os.path.exists(copies[0]), "the checked copy must be removed")
                self.assertTrue(os.path.exists(source))

    def test_original_ogg_is_kept(self):
        with tempfile.TemporaryDirectory() as directory:
            source = os.path.join(directory, "input.ogg")
            with open(source, "wb") as stream:
                stream.write(b"audio")
            with patch("whatsapp.requests.post", return_value=Mock(status_code=200, json=Mock(return_value={"success": True}))), patch(
                "whatsapp.audio.convert_to_opus_ogg_temp"
            ) as convert:
                self.assertTrue(whatsapp.send_audio_message("test", source)[0])
                convert.assert_not_called()
            self.assertTrue(os.path.exists(source))
