// A minimal MCP server with a single echo tool.

import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { z } from "zod";

/** Creates the server, so tests can connect to it without stdio. */
export function createServer(): McpServer {
    const server = new McpServer({ name: "echo", version: "1.0.0" });
    server.registerTool(
        "echo",
        {
            description: "Return the message unchanged.",
            inputSchema: { message: z.string() },
        },
        ({ message }) => ({ content: [{ type: "text", text: message }] }),
    );
    return server;
}

if (import.meta.main) {
    await createServer().connect(new StdioServerTransport());
}
