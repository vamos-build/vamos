// Syntax colors for code blocks, as VS Code themes for Expressive Code.
// They use only the site's palette (src/styles/vamos.css): calls in green, strings in marigold,
// keywords bold in the text color, comments muted. Starlight supplies the frame and background colors.
// Marigold fails as text on the light page, so light strings use a dark amber; every token meets WCAG 2.2 AA.

/**
 * @param {{name: string, type: "dark" | "light", text: string, strong: string, call: string, string: string,
 *   quiet: string}} c
 */
function theme(c) {
  return {
    name: c.name,
    type: c.type,
    colors: { "editor.foreground": c.text, "editor.background": "#00000000" },
    tokenColors: [
      { settings: { foreground: c.text } },
      { scope: ["comment", "punctuation.definition.comment"], settings: { foreground: c.quiet, fontStyle: "italic" } },
      { scope: ["punctuation", "keyword.operator", "meta.brace"], settings: { foreground: c.quiet } },
      { scope: ["string", "punctuation.definition.string"], settings: { foreground: c.string } },
      { scope: ["constant.numeric", "constant.language"], settings: { foreground: c.string } },
      {
        scope: ["keyword", "storage.type", "storage.modifier", "keyword.operator.logical"],
        settings: { foreground: c.strong, fontStyle: "bold" },
      },
      {
        scope: [
          "entity.name.function",
          "support.function",
          "meta.function-call.generic",
          "variable.function",
          "entity.name.command",
        ],
        settings: { foreground: c.call },
      },
      { scope: ["variable.parameter", "constant.other.option"], settings: { foreground: c.text } },
      // The shell grammar marks every bare argument as a string; only quoted strings should stand out.
      { scope: ["string.unquoted.argument"], settings: { foreground: c.text } },
    ],
  };
}

export const vamosDark = theme({
  name: "vamos-dark",
  type: "dark",
  text: "#c2cfc6",
  strong: "#e2ebe4",
  call: "#6fd3a0",
  string: "#f5c451",
  quiet: "#97ab9e",
});

export const vamosLight = theme({
  name: "vamos-light",
  type: "light",
  text: "#2e3d34",
  strong: "#14201a",
  call: "#1e6b47",
  string: "#8a5a00",
  quiet: "#4b5b51",
});
