import asyncio
import inspect
import threading
import unittest
from unittest.mock import patch

from test_mcp_modes import load_main


class ToolThreads(unittest.IsolatedAsyncioTestCase):
    async def test_read_and_write_leave_the_loop_free_for_other_tools(self):
        mod = load_main()
        for name, target, arguments, result in (
            ("search_contacts", "whatsapp_search_contacts", {"query": "test"}, []),
            ("send_message", "whatsapp_send_message", {"recipient": "test", "message": "test"}, (True, "sent")),
        ):
            with self.subTest(name):
                entered, release = threading.Event(), threading.Event()
                threads = []

                def blocking(*args, **kwargs):
                    threads.append(threading.get_ident())
                    entered.set()
                    release.wait(2)
                    return result

                with patch.object(mod, target, side_effect=blocking), patch.object(mod, "whatsapp_list_chats", return_value=[]):
                    task = asyncio.create_task(mod.mcp.call_tool(name, arguments))
                    try:
                        for _ in range(100):
                            if entered.is_set():
                                break
                            await asyncio.sleep(0.01)
                        self.assertTrue(entered.is_set())
                        self.assertNotEqual(threads, [threading.get_ident()])
                        await asyncio.wait_for(mod.mcp.call_tool("list_chats", {}), 1)
                        self.assertFalse(task.done(), "blocking tool finished before the loop could run another request")
                    finally:
                        release.set()
                        await task

    async def test_registration_keeps_schema_documentation_and_readonly_mode(self):
        mod = load_main(MCP_READONLY="true")
        tools = {tool.name: tool for tool in await mod.mcp.list_tools()}
        self.assertNotIn("send_message", tools)
        tool = tools["search_contacts"]
        self.assertEqual(inspect.cleandoc(tool.description), inspect.getdoc(mod.search_contacts))
        self.assertEqual(tool.inputSchema["required"], ["query"])
        self.assertEqual(tool.inputSchema["properties"]["query"]["type"], "string")
        self.assertTrue(tool.annotations.readOnlyHint)
