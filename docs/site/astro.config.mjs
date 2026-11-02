// @ts-check
import { defineConfig } from "astro/config";
import mermaid from "astro-mermaid";
import starlight from "@astrojs/starlight";
import { github, layout, remarkRepoLinks } from "./links.mjs";
import { sidebar } from "./sidebar.mjs";

export default defineConfig({
  site: "https://vamos.build",
  markdown: {
    remarkPlugins: [[remarkRepoLinks, layout(new URL("./", import.meta.url))]],
  },
  integrations: [
    // Must precede Starlight, so that Expressive Code does not render mermaid blocks as code.
    mermaid({ autoTheme: true }),
    starlight({
      title: "vamos",
      social: [{ icon: "github", label: "GitHub", href: github }],
      // Page paths are relative to this directory, as ../user/tutorial.md.
      editLink: { baseUrl: `${github}/edit/main/docs/site/` },
      routeMiddleware: "./src/routeData.ts",
      sidebar,
    }),
  ],
});
