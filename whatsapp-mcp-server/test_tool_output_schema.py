import asyncio
import unittest

from test_mcp_modes import load_main


class NoToolDeclaresAnOutputSchema(unittest.TestCase):
    """A declared output schema makes the SDK validate every result against the loose return hints."""

    def test_every_registered_tool(self):
        tools = asyncio.run(load_main().mcp.list_tools())
        self.assertTrue(tools)
        self.assertEqual([t.name for t in tools if t.outputSchema], [])


if __name__ == "__main__":
    unittest.main()
