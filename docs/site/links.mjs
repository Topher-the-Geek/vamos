import { existsSync, statSync } from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import remarkGfm from "remark-gfm";
import remarkParse from "remark-parse";
import { unified } from "unified";

export const github = "https://github.com/Topher-the-Geek/vamos";

/** Returns the directory of the user guide and of the repository, given the site's root URL. */
export function layout(root) {
  return {
    pages: fileURLToPath(new URL("../user/", root)),
    repo: fileURLToPath(new URL("../../", root)),
  };
}

/** Returns the slug of a page from its collection id: "" for the guide's index, "reference/cli" for a reference. */
export function slugOf(id) {
  return id.replace(/(^|\/)index$/, "");
}

export function htmlPath(slug) {
  return slug ? `/${slug}/` : "/";
}

export function markdownPath(slug) {
  return `/${slug || "index"}.md`;
}

/** Returns the collection id of a page from its path in the guide: "index" for a README, "reference/cli" for a reference. */
export function idOf(file) {
  return file.replace(/\.md$/, "").replace(/(^|\/)README$/, "$1index").toLowerCase();
}

/**
 * Resolves a relative link written to work on GitHub, in the file `from`, into a link that works on the site.
 * A link to a page, or to a directory of the guide holding a README, becomes `pagePath` of its slug,
 * keeping any query and fragment; a link of only a query or fragment is to `from` itself.
 * A link to a missing file throws, so a moved page cannot leave a broken link behind,
 * and so does a link to any other file: the guide links to those by their GitHub URL,
 * since the site is rebuilt only when the guide changes and could not notice the file moving.
 */
export function resolveLink(url, from, { pages, repo }, pagePath) {
  const [, target, suffix] = /^([^?#]*)(.*)$/s.exec(url);
  let abs;
  try {
    abs = target ? path.resolve(path.dirname(from), decodeURI(target)) : from;
  } catch {
    throw new Error(`${path.relative(repo, from)}: malformed link ${url}`);
  }
  if (!existsSync(abs)) {
    throw new Error(`${path.relative(repo, from)}: link to missing file ${target}`);
  }
  const isDirectory = statSync(abs).isDirectory();
  const page = isDirectory ? path.join(abs, "README.md") : abs;
  const inPages = path.relative(pages, page);
  if (inPages.startsWith("..") || !page.endsWith(".md") || !existsSync(page)) {
    throw new Error(`${path.relative(repo, from)}: link to ${target}, which is not a page; use its GitHub URL`);
  }
  return pagePath(slugOf(idOf(inPages))) + suffix;
}

export function isRelative(url) {
  return !/^([a-z][a-z0-9+.-]*:|#|\/)/i.test(url);
}

/** Calls `fn` on each link and link definition in a Markdown tree whose URL is relative. */
function visitRelativeLinks(tree, fn) {
  const visit = (node) => {
    if ((node.type === "link" || node.type === "definition") && isRelative(node.url)) fn(node);
    node.children?.forEach(visit);
  };
  visit(tree);
}

/** A remark plugin that resolves the relative links of the HTML pages. */
export function remarkRepoLinks(options) {
  return (tree, file) =>
    visitRelativeLinks(tree, (node) => {
      node.url = resolveLink(node.url, file.path, options, htmlPath);
    });
}

const parser = unified().use(remarkParse).use(remarkGfm);

/**
 * Resolves the relative links of Markdown source, so that a page links to the other pages' Markdown.
 * The source is parsed as the HTML pages are, so it finds the same links,
 * and only their destinations change, leaving the rest of the text as written.
 */
export function resolveMarkdownLinks(source, from, options) {
  const edits = [];
  visitRelativeLinks(parser.parse(source), (node) => {
    const [start, end] = destination(source, node);
    let url = resolveLink(node.url, from, options, markdownPath);
    if (/[\s()<>]/.test(url)) url = `<${url}>`;
    edits.push({ start, end, url });
  });
  return edits
    .sort((a, b) => b.start - a.start)
    .reduce((text, { start, end, url }) => text.slice(0, start) + url + text.slice(end), source);
}

/** Returns the source offsets of the destination of a link, `[text](here "title")`, or a definition, `[label]: here`. */
function destination(source, node) {
  let i;
  if (node.type === "link") {
    // The label closes at the first "](" after its last child.
    i = source.indexOf("](", node.children.at(-1)?.position.end.offset ?? node.position.start.offset) + 2;
  } else {
    i = node.position.start.offset + /^\[(?:\\.|[^\\\]])*\]:/.exec(source.slice(node.position.start.offset))[0].length;
  }
  while (/\s/.test(source[i])) i++;
  if (source[i] === "<") return [i, source.indexOf(">", i) + 1];
  let end = i;
  for (let depth = 0; end < source.length && !/\s/.test(source[end]); end++) {
    if (source[end] === "\\") end++;
    else if (source[end] === "(") depth++;
    else if (source[end] === ")" && depth-- === 0) break;
  }
  return [i, end];
}
