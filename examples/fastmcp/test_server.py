"""Tests for the echo MCP server, driven through an in-memory MCP client."""

from fastmcp import Client

from server import mcp


async def test_lists_echo_tool() -> None:
    async with Client(mcp) as client:
        tools = await client.list_tools()

    assert [tool.name for tool in tools] == ["echo"]


async def test_echo_returns_message() -> None:
    async with Client(mcp) as client:
        result = await client.call_tool("echo", {"message": "hola"})

    assert not result.is_error
    assert result.data == "hola"
