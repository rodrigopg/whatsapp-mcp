import unittest
from unittest.mock import MagicMock, patch

import whatsapp


def resp(body):
    r = MagicMock()
    r.json.return_value = body
    return r


class TestGroupTools(unittest.TestCase):
    def test_invite_get_uses_get_and_reset_uses_post(self):
        with patch("whatsapp._api_request", return_value=resp({"success": True, "message": "ok", "link": "L"})) as m:
            self.assertEqual(whatsapp.get_group_invite_link("1@g.us"), (True, "ok", "L"))
            self.assertEqual(m.call_args.args, ("GET", "/group_invite"))
            whatsapp.get_group_invite_link("1@g.us", reset=True)
            self.assertEqual(m.call_args.args, ("POST", "/group_invite_reset"))

    def test_invite_failure_returns_no_link(self):
        with patch("whatsapp._api_request", return_value=resp({"success": False, "message": "no", "link": "L"})):
            self.assertEqual(whatsapp.get_group_invite_link("1@g.us"), (False, "no", None))

    def test_required_args(self):
        self.assertFalse(whatsapp.get_group_invite_link(" ")[0])
        self.assertFalse(whatsapp.join_group_with_link("")[0])
        self.assertFalse(whatsapp.update_group_settings("1@g.us")[0])

    def test_settings_sends_only_given_fields_including_false(self):
        with patch("whatsapp._api_request", return_value=resp({"success": True, "message": "ok"})) as m:
            whatsapp.update_group_settings("1@g.us", announce=False, topic="")
            self.assertEqual(m.call_args.kwargs["json"], {"group_jid": "1@g.us", "topic": "", "announce": False})


if __name__ == "__main__":
    unittest.main()
