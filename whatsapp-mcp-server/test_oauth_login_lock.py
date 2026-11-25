import os
import tempfile
import unittest
from unittest.mock import Mock

from starlette.requests import Request

from oauth import MAX_CLIENTS, MAX_FAILS, OwnerOAuth, client_key, install_oauth_routes

PASS = "p" * 32


class LoginLockPerClient(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.clock = [1000.0]
        self.provider = OwnerOAuth(os.path.join(self.tmp.name, "o.db"), PASS, "", "https://mcp.example.com", now=lambda: self.clock[0])
        self.addCleanup(self.provider._db.close)

    def test_wrong_passphrases_lock_only_the_address_that_typed_them(self):
        for _ in range(MAX_FAILS + 5):
            self.provider.check_passphrase("wrong", "203.0.113.7")
        self.assertTrue(self.provider.locked("203.0.113.7"))
        self.assertFalse(self.provider.locked("203.0.113.8"), "another visitor must still be able to sign in")
        self.assertTrue(self.provider.check_passphrase(PASS, "203.0.113.8"))
        self.clock[0] += 901
        self.assertFalse(self.provider.locked("203.0.113.7"), "the lock ends with the window")

    def test_one_ipv6_network_is_one_client(self):
        self.assertEqual(client_key("2001:db8:1:2::a"), client_key("2001:db8:1:2:ffff::1"))
        self.assertNotEqual(client_key("2001:db8:1:2::a"), client_key("2001:db8:1:3::a"))
        self.assertEqual(client_key("::ffff:203.0.113.7"), client_key("203.0.113.7"))
        self.assertEqual(client_key("testclient"), "testclient")
        self.assertEqual(client_key(""), "")

    def test_the_table_of_clients_is_bounded(self):
        for i in range(MAX_CLIENTS + 50):
            self.provider.check_passphrase("wrong", f"10.{i // 65536}.{i // 256 % 256}.{i % 256}")
        self.assertLessEqual(len(self.provider._fails), MAX_CLIENTS + 1)


class ForwardedAddressTrust(unittest.TestCase):
    def test_the_server_takes_a_forwarded_address_only_from_a_proxy_on_this_machine(self):
        """request.client is what the lock counts: uvicorn rewrites it from X-Forwarded-For for loopback peers only."""
        from unittest.mock import patch
        from uvicorn.config import Config
        with patch.dict(os.environ):
            os.environ.pop("FORWARDED_ALLOW_IPS", None)
            config = Config(app=None)
        self.assertTrue(config.proxy_headers)
        self.assertEqual({h.strip() for h in config.forwarded_allow_ips.split(",")}, {"127.0.0.1", "::1"})


class LoginRouteUsesTheClientAddress(unittest.IsolatedAsyncioTestCase):
    async def test_the_lock_is_asked_for_the_requesting_address(self):
        routes = {}

        class Routes:
            def custom_route(self, path, **kwargs):
                return lambda fn: routes.setdefault(path, fn)

        provider = Mock()
        provider.locked.return_value = True
        install_oauth_routes(Routes(), provider)

        async def receive():
            return {"type": "http.request", "body": b"req=x&passphrase=wrong", "more_body": False}

        request = Request({"type": "http", "method": "POST", "path": "/oauth/login", "headers": [], "client": ("2001:db8:1:2::a", 4000)}, receive)
        result = await routes["/oauth/login"](request)
        self.assertEqual(result.status_code, 429)
        provider.locked.assert_called_once_with("2001:db8:1:2::/64")


if __name__ == "__main__":
    unittest.main()
