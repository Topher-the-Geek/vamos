import type { APIRoute } from "astro";
import { markdown, markdownResponse, pages } from "../markdown";

// Every page's Markdown in one file, in sidebar order.
export const GET: APIRoute = async () =>
  markdownResponse((await pages()).map(markdown).join("\n"), "text/plain");
