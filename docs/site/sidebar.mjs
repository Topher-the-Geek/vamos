/** The sidebar, which also orders the pages in llms.txt. */
export const sidebar = [
  { label: "User guide", link: "/" },
  "tutorial",
  {
    label: "Reference",
    items: ["reference/build", "reference/cli", "reference/configuration", "reference/cache"],
  },
];

/** The slugs of the sidebar's pages, in order, with "" for the index. */
export const pageOrder = sidebar.flatMap(function slugs(item) {
  if (typeof item === "string") return [item];
  if (item.items) return item.items.flatMap(slugs);
  return [item.link.replace(/^\/|\/$/g, "")];
});
