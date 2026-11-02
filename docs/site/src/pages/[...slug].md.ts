import type { APIRoute, GetStaticPaths, InferGetStaticPropsType } from "astro";
import { slugOf } from "../../links.mjs";
import { markdown, markdownResponse, pages } from "../markdown";

// Each page's Markdown, at its path with .md: /tutorial.md, and /index.md for the index.
export const getStaticPaths = (async () =>
  (await pages()).map((entry) => ({
    params: { slug: slugOf(entry.id) || "index" },
    props: { entry },
  }))) satisfies GetStaticPaths;

export const GET: APIRoute<InferGetStaticPropsType<typeof getStaticPaths>> = ({ props }) =>
  markdownResponse(markdown(props.entry));
