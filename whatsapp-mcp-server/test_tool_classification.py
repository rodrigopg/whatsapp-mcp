"""Every MCP tool must be registered through read_tool / write_tool.

MCP_READONLY hides write tools by never registering them. A tool registered with a bare `@mcp.tool()` would stay
visible in read-only mode (and over the remote transport), so a new tool, for example from a third-party PR, could
silently bypass it. This test fails the moment that happens.
"""
import ast
import os
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))


def _decorator_names(fn):
    names = []
    for d in fn.decorator_list:
        node = d.func if isinstance(d, ast.Call) else d
        if isinstance(node, ast.Attribute):
            names.append(f"{ast.unparse(node.value)}.{node.attr}")
        elif isinstance(node, ast.Name):
            names.append(node.id)
    return names


class ToolClassification(unittest.TestCase):
    def setUp(self):
        with open(os.path.join(HERE, "main.py")) as f:
            self.tree = ast.parse(f.read())

    def test_no_bare_mcp_tool_decorator(self):
        offenders = [fn.name for fn in ast.walk(self.tree)
                     if isinstance(fn, (ast.FunctionDef, ast.AsyncFunctionDef))
                     and "mcp.tool" in _decorator_names(fn)]
        self.assertEqual(offenders, [], f"register these with @read_tool or @write_tool: {offenders}")

    def test_every_tool_is_classified_exactly_once(self):
        tools = [fn.name for fn in ast.walk(self.tree) if isinstance(fn, ast.FunctionDef)
                 and any(n in ("read_tool", "write_tool") for n in _decorator_names(fn))]
        self.assertTrue(tools, "no tools found: did main.py move?")
        self.assertEqual(len(tools), len(set(tools)), "a tool is defined twice")
        for fn in ast.walk(self.tree):
            if isinstance(fn, ast.FunctionDef):
                names = _decorator_names(fn)
                self.assertFalse("read_tool" in names and "write_tool" in names, f"{fn.name} is both read and write")


if __name__ == "__main__":
    unittest.main()
