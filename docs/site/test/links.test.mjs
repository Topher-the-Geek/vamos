import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { after, test } from "node:test";
import { resolveMarkdownLinks } from "../links.mjs";

const repo = mkdtempSync(path.join(tmpdir(), "links-"));
after(() => rmSync(repo, { recursive: true, force: true }));
const pages = path.join(repo, "docs/user");
mkdirSync(path.join(pages, "reference"), { recursive: true });
for (const file of ["README.md", "tutorial.md", "reference/cli.md"]) writeFileSync(path.join(pages, file), "");
writeFileSync(path.join(repo, "main.go"), "");
const from = path.join(pages, "tutorial.md");
const resolve = (source) => resolveMarkdownLinks(source, from, { pages, repo });

test("links to pages go to their Markdown", () => {
  assert.equal(
    resolve("See [the CLI](reference/cli.md#flags) and [home](README.md)."),
    "See [the CLI](/reference/cli.md#flags) and [home](/index.md).",
  );
});

test("a relative link to a file that is not a page throws", () => {
  assert.throws(() => resolve("[main](../../main.go)"), /link to \.\.\/\.\.\/main\.go, which is not a page; use its GitHub URL/);
  assert.throws(() => resolve("[ref](reference/)"), /link to reference\/, which is not a page/);
});

test("absolute and external links are left alone", () => {
  const source = "[a](https://x.dev/missing.md) [b](/missing.md) [c](#missing)";
  assert.equal(resolve(source), source);
});

test("titled, angle-bracketed and reference links are resolved", () => {
  assert.equal(
    resolve('[a](reference/cli.md "The CLI") [b](<reference/cli.md>) [c][cli]\n\n[cli]: reference/cli.md "CLI"\n'),
    '[a](/reference/cli.md "The CLI") [b](/reference/cli.md) [c][cli]\n\n[cli]: /reference/cli.md "CLI"\n',
  );
});

test("links in code are not links", () => {
  const source = [
    "Write `[example](missing.md)` inline.",
    "",
    "````md",
    "```",
    "[example](missing.md)",
    "```",
    "[example](missing.md)",
    "````",
    "",
    "    [example](missing.md)",
    "",
  ].join("\n");
  assert.equal(resolve(source), source);
});

test("a link label holding code or brackets keeps its text", () => {
  assert.equal(resolve("[`a](b)` \\] text](reference/cli.md)"), "[`a](b)` \\] text](/reference/cli.md)");
});

test("a link to a missing file throws", () => {
  assert.throws(() => resolve("[gone](gone.md)"), /link to missing file gone\.md/);
});

test("a query and every fragment are kept", () => {
  assert.equal(resolve("[a](reference/cli.md?plain=1#L3) [b](reference/cli.md#one#two)"), "[a](/reference/cli.md?plain=1#L3) [b](/reference/cli.md#one#two)");
});

test("a link to a directory goes to its README's page", () => {
  assert.equal(resolve("[home](./)"), "[home](/index.md)");
});

test("a malformed link throws, naming the file and the link", () => {
  assert.throws(() => resolve("[bad](%FF.md)"), /docs\/user\/tutorial\.md: malformed link %FF\.md/);
});

test("a link of only a query goes to its own page", () => {
  assert.equal(resolve("[same](?plain=1#x)"), "[same](/tutorial.md?plain=1#x)");
});
