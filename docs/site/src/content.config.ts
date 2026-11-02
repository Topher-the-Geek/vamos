import { defineCollection } from "astro:content";
import { glob } from "astro/loaders";
import { docsSchema } from "@astrojs/starlight/schema";
import { idOf } from "../links.mjs";

// The pages are the Markdown files in docs/user, which stay readable on GitHub;
// a README is its directory's index page.
export const collections = {
  docs: defineCollection({
    loader: glob({
      base: "../user",
      pattern: "**/*.md",
      generateId: ({ entry }) => idOf(entry),
    }),
    schema: docsSchema(),
  }),
};
