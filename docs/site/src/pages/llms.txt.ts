import type { APIRoute } from "astro";
import { markdownPath, slugOf } from "../../links.mjs";
import { markdownResponse, pages } from "../markdown";

// An index of the pages for agents, as https://llmstxt.org describes.
export const GET: APIRoute = async ({ site }) => {
  const url = (p: string) => (site ? new URL(p, site).href : p);
  const list = (await pages()).map(
    ({ id, data }) => `- [${data.title}](${url(markdownPath(slugOf(id)))})${data.description ? `: ${data.description}` : ""}`,
  );
  return markdownResponse(
    `# vamos

> A Go CLI that uses the buildah library to build container images described by BUILD files written in Starlark.

Every page is Markdown at its URL with \`.md\` in place of the trailing slash, and the home page at \`/index.md\`,
or at its URL itself when requested with \`Accept: text/markdown\`.
[llms-full.txt](${url("/llms-full.txt")}) holds every page in one file.

## User guide

${list.join("\n")}
`,
    "text/plain",
  );
};
