// @ts-check
import { defineConfig } from "astro/config";
import mermaid from "astro-mermaid";
import starlight from "@astrojs/starlight";
import { github, layout, remarkRepoLinks } from "./links.mjs";
import { sidebar } from "./sidebar.mjs";
import { vamosDark, vamosLight } from "./src/code-themes.mjs";

export default defineConfig({
  site: "https://vamos.build",
  markdown: {
    remarkPlugins: [[remarkRepoLinks, layout(new URL("./", import.meta.url))]],
  },
  integrations: [
    // Must precede Starlight, so that Expressive Code does not render mermaid blocks as code.
    // Mermaid lays out labels in this font, so it must be the one the page renders them in; colors are in vamos.css.
    mermaid({
      autoTheme: true,
      mermaidConfig: { themeVariables: { fontFamily: '"Figtree Variable", Figtree, system-ui, sans-serif' } },
    }),
    starlight({
      title: "vamos",
      logo: { light: "./src/assets/logo-light.svg", dark: "./src/assets/logo-dark.svg", replacesTitle: true },
      favicon: "/favicon.svg",
      expressiveCode: {
        themes: [vamosDark, vamosLight],
        // The themes leave their frames and background to Starlight, whose palette vamos.css sets.
        useStarlightUiThemeColors: true,
      },
      customCss: [
        "@fontsource-variable/bricolage-grotesque",
        "@fontsource-variable/figtree",
        "@fontsource-variable/martian-mono",
        "./src/styles/vamos.css",
      ],
      social: [{ icon: "github", label: "GitHub", href: github }],
      // Page paths are relative to this directory, as ../user/tutorial.md.
      editLink: { baseUrl: `${github}/edit/main/docs/site/` },
      routeMiddleware: "./src/routeData.ts",
      sidebar,
    }),
  ],
});
