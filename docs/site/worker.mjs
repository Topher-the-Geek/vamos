/**
 * Serves the site's pages, giving a page's Markdown instead of its HTML to a client that prefers it,
 * as an agent asking with `Accept: text/markdown` does.
 * It runs for every request but those for scripts, styles and the search index, which go straight to their assets.
 */
export default {
  async fetch(request, env) {
    let response;
    const url = new URL(request.url);
    // Only a GET or HEAD has no body, which the Markdown request would use up before any fallback.
    const read = request.method === "GET" || request.method === "HEAD";
    if (read && prefersMarkdown(request.headers.get("Accept") ?? "") && !/\.[^/]*$/.test(url.pathname)) {
      url.pathname = (url.pathname.replace(/\/$/, "") || "/index") + ".md";
      const markdown = await env.ASSETS.fetch(new Request(url, request));
      // Any answer but a missing page stands, so that a 304 revalidates the Markdown rather than fetching the HTML.
      if (markdown.status !== 404) response = markdown;
      else await markdown.body?.cancel();
    }
    response ??= await env.ASSETS.fetch(request);
    response = new Response(response.body, response);
    response.headers.append("Vary", "Accept");
    return response;
  },
};

/**
 * Reports whether an Accept header prefers text/markdown to text/html.
 * Each type takes the quality of its most specific matching range, as RFC 9110 says.
 * A tie goes to Markdown only when the header names text/markdown itself, so `*\/*` alone gets HTML.
 */
export function prefersMarkdown(accept) {
  const ranges = accept
    .split(",")
    .map((range) => {
      const [name, ...params] = range.split(";").map((s) => s.trim().toLowerCase());
      const q = params.find((p) => p.startsWith("q="));
      return { name, q: q ? Number(q.slice(2)) : 1 };
    })
    .filter(({ name, q }) => name && !Number.isNaN(q));
  const match = (type) => {
    const specificity = (name) => (name === type ? 3 : name === `${type.split("/")[0]}/*` ? 2 : name === "*/*" ? 1 : 0);
    let best = { q: 0, specificity: 0 };
    for (const { name, q } of ranges) {
      const s = specificity(name);
      if (s > best.specificity) best = { q, specificity: s };
    }
    return best;
  };
  const markdown = match("text/markdown");
  const html = match("text/html");
  return markdown.q > html.q || (markdown.q > 0 && markdown.q === html.q && markdown.specificity === 3);
}
