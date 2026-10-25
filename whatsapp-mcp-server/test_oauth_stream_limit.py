import unittest
from unittest.mock import Mock

from starlette.requests import Request

from oauth import install_oauth_routes


class OAuthStreamLimit(unittest.IsolatedAsyncioTestCase):
    async def login(self, chunks):
        routes = {}

        class Routes:
            def custom_route(self, path, **kwargs):
                def register(fn):
                    routes[path] = fn
                    return fn
                return register

        provider = Mock()
        provider.locked.return_value = True
        install_oauth_routes(Routes(), provider)
        consumed = []

        async def receive():
            index = len(consumed)
            if index >= len(chunks):
                self.fail("login consumed the body after crossing its size limit")
            consumed.append(index)
            return {"type": "http.request", "body": chunks[index], "more_body": index < len(chunks) - 1}

        request = Request({"type": "http", "method": "POST", "path": "/oauth/login", "headers": []}, receive)
        result = await routes["/oauth/login"](request)
        return result, consumed, provider

    async def test_chunked_body_stops_at_limit_without_reading_the_tail(self):
        result, consumed, provider = await self.login([b"x" * 2048, b"y" * 2048, b"z", b"unread tail"])
        self.assertEqual(result.status_code, 413)
        self.assertEqual(consumed, [0, 1, 2])
        provider.locked.assert_not_called()

    async def test_exact_limit_is_allowed_through_to_login(self):
        result, consumed, provider = await self.login([b"x" * 2048, b"y" * 2048])
        self.assertEqual(result.status_code, 429)
        self.assertEqual(consumed, [0, 1])
        provider.locked.assert_called_once()
