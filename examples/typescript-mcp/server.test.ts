// Tests for the echo MCP server, driven through an in-memory MCP client.

import assert from "node:assert/strict";
import { test } from "node:test";

import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { InMemoryTransport } from "@modelcontextprotocol/sdk/inMemory.js";

import { createServer } from "./server.ts";

/** Connects a client to a new server and closes both when the test ends. */
async function connect(t: test.TestContext): Promise<Client> {
    const [clientTransport, serverTransport] = InMemoryTransport.createLinkedPair();
    const server = createServer();
    const client = new Client({ name: "test", version: "1.0.0" });
    await server.connect(serverTransport);
    await client.connect(clientTransport);
    t.after(() => client.close());
    return client;
}

await test("lists the echo tool", async (t) => {
    const client = await connect(t);

    const { tools } = await client.listTools();

    assert.deepEqual(
        tools.map((tool) => tool.name),
        ["echo"],
    );
});

await test("echo returns the message", async (t) => {
    const client = await connect(t);

    const result = await client.callTool({ name: "echo", arguments: { message: "hola" } });

    assert.equal(result.isError, undefined);
    assert.deepEqual(result.content, [{ type: "text", text: "hola" }]);
});
