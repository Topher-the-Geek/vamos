# Documentation ownership

Use the narrowest scope that owns a fact.
Link to the owner when another scope needs it.

| Scope | Home | Content |
| --- | --- | --- |
| Symbol | Go doc comment immediately above the declaration | Preconditions, concurrency, errors and ownership |
| Package | Its `doc.go` package comment | Responsibility, package-wide rules, lifecycle and navigation to symbols |
| Repository | An atomic file in this directory | Phase boundaries, shared domain concepts, contributor workflows |
| Routing | [AGENTS.md](../../AGENTS.md) | Task/path to documentation links only |
| User workflow | [docs/user/](../../docs/user/README.md), example project READMEs | Tutorial, references and runnable project setup |

Do not add Go package READMEs.
The root [README.md](../../README.md) introduces vamos and links to the user guide and contributor instructions.
The [tutorial](../../docs/user/tutorial.md) owns the introductory walkthrough;
the references in `docs/user/reference/` own CLI, BUILD language, configuration and cache behavior.
The [example index](../../examples/README.md) helps users choose projects and build patterns.
Example project READMEs own project-specific setup and checks, including non-Go toolchains,
and link to the tutorial for shared vamos setup.

[docs/site](../../docs/site/astro.config.mjs) publishes `docs/user/` as a Starlight site on Cloudflare Workers;
each Markdown file there is a page, and a README is its directory's index.
Start each page with `title` and `description` frontmatter instead of a `#` heading,
which Starlight renders from the title.
Add a new page to the sidebar in `sidebar.mjs`, which also orders `llms.txt`.
Link to another page with a relative link, which works on GitHub and which the site turns into the page's URL.
Link to any other repository file by its GitHub URL, `https://github.com/Topher-the-Geek/vamos/blob/main/<path>`;
the build rejects a relative link to one.
Agents get the same pages as Markdown: at each page's path with `.md`, in `llms.txt` and `llms-full.txt`,
and at the page's own URL when they ask with `Accept: text/markdown`, which `worker.mjs` answers.
Cloudflare watches `docs/` and builds and deploys the site itself, so no CI workflow builds it,
and a change outside `docs/` never rebuilds it; that is why the guide cannot link to other files relatively.
After changing the user guide, run `pnpm install && pnpm build` in `docs/site`;
the build fails on a link to a missing file or to a file that is not a page, but does not check anchors.
After changing `worker.mjs` or `links.mjs`, also run `pnpm test` there,
which checks the Worker's content negotiation and the links of the Markdown pages.
Run `pnpm dev` there to preview with live reload, or `pnpm preview` to serve the build as Cloudflare does;
both listen on all interfaces, since VS Code cannot forward a port bound only to the container's loopback.

The root command and Go example also have `doc.go`.
[features](../../internal/features/) holds only tests, so its package comment is in `features_test.go`.
`internal/` contains only a dependency test, so its architectural context belongs in [architecture.md](architecture.md),
without adding a production package just for documentation.

Write for engineers familiar with Go and OCI.
Explain constraints and rejected alternatives when they affect maintenance.
Do not copy symbol comments, implementation snippets or exhaustive field lists into higher levels.
Use relative Markdown links in the knowledge base and precise identifiers in Go comments;
Go doc links such as `[Graph]` should name symbols available to that package.
Avoid Markdown link syntax in Go comments.

Wrap all prose, including code comments, Markdown and other documentation, within 120 columns;
count a tab as four columns.
Prefer one sentence per line.
When a sentence needs multiple lines, break at the highest point in its syntax: between clauses first,
then between phrases, and never inside a phrase.
Do not separate a noun from its relative clause, a verb from its object, a preposition from what it governs,
or a short pair joined by "and" or "or".
Choose those boundaries rather than filling each line to the limit, even when a line could hold more.
Keep URLs, paths, identifiers, commands, Markdown tables and other indivisible technical items intact;
allow them to exceed 120 columns when needed.
Preserve Markdown structures, comment directives and literal examples.

Each `doc.go` follows this shape, with sections omitted when they add no information:

```go
// Package name <states its responsibility and boundary>.
//
// # Invariants
//
// <Rules shared by the package; refer to symbol docs for individual contracts.>
//
// # Lifecycle
//
// <Package-level phases or ownership transitions, including failure paths.>
//
// # Changes
//
// <Entry points and coupled symbols a maintainer should inspect.>
package name
```

When behavior changes, update its owner and any affected routing links in the same change.
Add a knowledge-base file only for a distinct cross-cutting subject; do not create a second directory index.
Check local links and inspect `go doc ./internal/<package>` after changing package comments.
