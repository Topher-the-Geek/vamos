"""A minimal MCP server with a single echo tool."""

from fastmcp import FastMCP

mcp = FastMCP("echo", version="1.0.0")


@mcp.tool
def echo(message: str) -> str:
    """Return the message unchanged."""
    return message


if __name__ == "__main__":
    # The banner also checks PyPI for updates, which a pinned image can't use.
    mcp.run(show_banner=False)
