import { defineRouteMiddleware } from "@astrojs/starlight/route-data";
import { markdownPath, slugOf } from "../links.mjs";

// Points each page at its Markdown, for an agent that reads the HTML first.
export const onRequest = defineRouteMiddleware(({ locals }) => {
  const { entry, head } = locals.starlightRoute;
  if (entry.id === "404") return;
  head.push({ tag: "link", attrs: { rel: "alternate", type: "text/markdown", href: markdownPath(slugOf(entry.id)) } });
});
