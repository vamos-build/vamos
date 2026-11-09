/** The sidebar, which also orders the pages in llms.txt. */
export const sidebar = [
  { label: "User guide", link: "/" },
  "tutorial",
  {
    label: "Reference",
    items: [
      {
        label: "BUILD language",
        items: [
          { label: "Overview", link: "/reference/build/" },
          "reference/build/pull",
          "reference/build/glob",
          "reference/build/task",
          "reference/build/layer",
          "reference/build/image-index",
          "reference/build/publication",
          "reference/build/steps",
          "reference/build/secrets",
        ],
      },
      {
        label: "CLI",
        items: [
          { label: "Overview", link: "/reference/cli/" },
          "reference/cli/build",
          "reference/cli/push",
          "reference/cli/publish",
          "reference/cli/load",
          "reference/cli/save",
          "reference/cli/export",
          "reference/cli/update",
          "reference/cli/prune",
          "reference/cli/coordinator",
          "reference/cli/worker",
          "reference/cli/flags",
          "reference/cli/registry-credentials",
          "reference/cli/output",
        ],
      },
      "reference/configuration",
      "reference/cache",
      "reference/remote",
    ],
  },
];

/** The slugs of the sidebar's pages, in order, with "" for the index. */
export const pageOrder = sidebar.flatMap(function slugs(item) {
  if (typeof item === "string") return [item];
  if (item.items) return item.items.flatMap(slugs);
  return [item.link.replace(/^\/|\/$/g, "")];
});
