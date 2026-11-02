import assert from "node:assert/strict";
import { test } from "node:test";
import worker, { prefersMarkdown } from "../worker.mjs";

/** Returns an ASSETS binding serving `files` by path, answering 304 when If-None-Match carries the file's ETag. */
function assets(files) {
  const requests = [];
  return {
    requests,
    async fetch(request) {
      const { pathname } = new URL(request.url);
      requests.push(pathname);
      const body = files[pathname];
      if (body === undefined) return new Response("not found page", { status: 404 });
      const etag = `"${pathname}"`;
      if (request.headers.get("If-None-Match") === etag) return new Response(null, { status: 304, headers: { ETag: etag } });
      return new Response(body, { headers: { ETag: etag } });
    },
  };
}

const files = { "/guide/": "<html>", "/guide.md": "# Guide", "/": "<html>", "/index.md": "# Home", "/html-only/": "<html>" };

async function get(path, headers, init = {}) {
  const env = { ASSETS: assets(files) };
  const response = await worker.fetch(new Request(`https://example.com${path}`, { headers, ...init }), env);
  return { response, text: await response.text(), requests: env.ASSETS.requests };
}

test("an agent asking for Markdown gets the page's Markdown", async () => {
  const { response, text } = await get("/guide/", { Accept: "text/markdown" });
  assert.equal(text, "# Guide");
  assert.match(response.headers.get("Vary"), /Accept/);
});

test("the root page's Markdown is index.md", async () => {
  assert.equal((await get("/", { Accept: "text/markdown" })).text, "# Home");
});

test("a browser gets the HTML", async () => {
  const { response, text } = await get("/guide/", { Accept: "text/html,application/xhtml+xml,*/*;q=0.8" });
  assert.equal(text, "<html>");
  assert.match(response.headers.get("Vary"), /Accept/);
});

test("a Markdown revalidation answers 304 rather than fetching the HTML", async () => {
  const { response, requests } = await get("/guide/", { Accept: "text/markdown", "If-None-Match": '"/guide.md"' });
  assert.equal(response.status, 304);
  assert.deepEqual(requests, ["/guide.md"]);
});

test("a page without Markdown falls back to its HTML", async () => {
  const { response, text } = await get("/html-only/", { Accept: "text/markdown" });
  assert.equal(response.status, 200);
  assert.equal(text, "<html>");
});

test("a request with a body goes to its asset unchanged", async () => {
  const { response, requests } = await get("/html-only/", { Accept: "text/markdown" }, { method: "POST", body: "x" });
  assert.equal(response.status, 200);
  assert.deepEqual(requests, ["/html-only/"]);
});

test("a request for a file is not rewritten", async () => {
  const { requests } = await get("/guide.md", { Accept: "text/markdown" });
  assert.deepEqual(requests, ["/guide.md"]);
});

test("Accept negotiation ranks each type by its most specific range", () => {
  const cases = {
    "text/markdown": true,
    "text/markdown, text/html": true,
    "text/html, text/markdown": true,
    "text/markdown;q=0.5, text/html": false,
    "text/markdown;q=0.5, text/*;q=1": false,
    "text/markdown, text/*;q=0.5": true,
    "text/markdown;q=0, */*": false,
    "TEXT/MARKDOWN": true,
    "*/*": false,
    "text/*": false,
    "text/html, */*;q=0.8": false,
    "": false,
  };
  for (const [accept, expected] of Object.entries(cases)) {
    assert.equal(prefersMarkdown(accept), expected, accept);
  }
});
