import path from "node:path";
import { fileURLToPath } from "node:url";
import { root } from "astro:config/server";
import { getCollection, type CollectionEntry } from "astro:content";
import { layout, resolveMarkdownLinks, slugOf } from "../links.mjs";
import { pageOrder } from "../sidebar.mjs";

/** Returns the pages in sidebar order, then any the sidebar leaves out, without drafts in a production build, as Starlight does. */
export async function pages(): Promise<CollectionEntry<"docs">[]> {
  const rank = (entry: CollectionEntry<"docs">) => {
    const i = pageOrder.indexOf(slugOf(entry.id));
    return i < 0 ? pageOrder.length : i;
  };
  const entries = await getCollection("docs", ({ data }) => !(import.meta.env.PROD && data.draft));
  return entries.sort((a, b) => rank(a) - rank(b) || a.id.localeCompare(b.id));
}

/** Returns a page as Markdown for agents: its title and description, then its source with links to Markdown. */
export function markdown(entry: CollectionEntry<"docs">): string {
  const { title, description } = entry.data;
  const file = path.resolve(fileURLToPath(root), entry.filePath!);
  const body = resolveMarkdownLinks(entry.body!, file, layout(root));
  return `# ${title}\n\n${description ? `> ${description}\n\n` : ""}${body.trim()}\n`;
}

export function markdownResponse(text: string, type = "text/markdown"): Response {
  return new Response(text, { headers: { "Content-Type": `${type}; charset=utf-8` } });
}
