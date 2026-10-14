/**
 * Terminal colour themes. Each is the theme's published palette: background, foreground,
 * cursor, selection and the 16 ANSI colours (0–7 normal, 8–15 bright).
 *
 * Each theme is applied twice: as CSS variables for what wterm draws itself (default text,
 * background, selection) and as escape sequences inside Ghostty's core for the ANSI palette
 * (`paletteSequence`). Both switch at once, with no reconnect.
 */
export type TerminalTheme = {
  id: string
  name: string
  appearance: "dark" | "light"
  background: string
  foreground: string
  cursor: string
  selection: string
  ansi: readonly string[]
}

const theme = (
  id: string,
  name: string,
  appearance: "dark" | "light",
  [background, foreground, cursor, selection]: [string, string, string, string],
  ansi: string,
): TerminalTheme => ({ id, name, appearance, background, foreground, cursor, selection, ansi: ansi.split(" ") })

export const terminalThemes: readonly TerminalTheme[] = [
  // The Paperboat pair is built from the console's colours (app/globals.css): its background,
  // text and neutral greys; brand blue for blue, the cursor and a blue selection; and its red,
  // green and amber. The other hues share their hue family's lightness and chroma in OKLCH, so
  // no colour stands out. Normal colours read at 4.5:1 or better on the background.
  theme(
    "paperboat-dark",
    "Paperboat Dark",
    "dark",
    ["#121212", "#f5f5f5", "#4275f8", "#203566"],
    "#2e2e2e #f75d5c #22c373 #f7ac4d #4275f8 #bb8aef #04bbc3 #c4c4c4 #808080 #ffaba3 #68e099 #fad779 #a8c4fe #d5b2ff #10dee7 #f5f5f5",
  ),
  theme(
    "paperboat-light",
    "Paperboat Light",
    "light",
    ["#ffffff", "#0d0d0d", "#1d52eb", "#cedeff"],
    "#0d0d0d #dc2627 #057743 #855904 #1d52eb #7a41af #057176 #6c6c6c #555555 #e64343 #069253 #a46e01 #4275f8 #925ac9 #048b91 #868686",
  ),
  theme(
    "dracula",
    "Dracula",
    "dark",
    ["#282a36", "#f8f8f2", "#f8f8f2", "#44475a"],
    "#21222c #ff5555 #50fa7b #f1fa8c #bd93f9 #ff79c6 #8be9fd #f8f8f2 #6272a4 #ff6e6e #69ff94 #ffffa5 #d6acff #ff92df #a4ffff #ffffff",
  ),
  theme(
    "catppuccin-mocha",
    "Catppuccin Mocha",
    "dark",
    ["#1e1e2e", "#cdd6f4", "#f5e0dc", "#45475a"],
    "#45475a #f38ba8 #a6e3a1 #f9e2af #89b4fa #f5c2e7 #94e2d5 #bac2de #585b70 #f38ba8 #a6e3a1 #f9e2af #89b4fa #f5c2e7 #94e2d5 #a6adc8",
  ),
  theme(
    "catppuccin-macchiato",
    "Catppuccin Macchiato",
    "dark",
    ["#24273a", "#cad3f5", "#f4dbd6", "#494d64"],
    "#494d64 #ed8796 #a6da95 #eed49f #8aadf4 #f5bde6 #8bd5ca #b8c0e0 #5b6078 #ed8796 #a6da95 #eed49f #8aadf4 #f5bde6 #8bd5ca #a5adcb",
  ),
  theme(
    "catppuccin-frappe",
    "Catppuccin Frappé",
    "dark",
    ["#303446", "#c6d0f5", "#f2d5cf", "#51576d"],
    "#51576d #e78284 #a6d189 #e5c890 #8caaee #f4b8e4 #81c8be #b5bfe2 #626880 #e78284 #a6d189 #e5c890 #8caaee #f4b8e4 #81c8be #a5adce",
  ),
  theme(
    "catppuccin-latte",
    "Catppuccin Latte",
    "light",
    ["#eff1f5", "#4c4f69", "#dc8a78", "#ccd0da"],
    "#5c5f77 #d20f39 #40a02b #df8e1d #1e66f5 #ea76cb #179299 #acb0be #6c6f85 #d20f39 #40a02b #df8e1d #1e66f5 #ea76cb #179299 #bcc0cc",
  ),
  theme(
    "tokyo-night",
    "Tokyo Night",
    "dark",
    ["#1a1b26", "#c0caf5", "#c0caf5", "#283457"],
    "#15161e #f7768e #9ece6a #e0af68 #7aa2f7 #bb9af7 #7dcfff #a9b1d6 #414868 #f7768e #9ece6a #e0af68 #7aa2f7 #bb9af7 #7dcfff #c0caf5",
  ),
  theme(
    "rose-pine",
    "Rosé Pine",
    "dark",
    ["#191724", "#e0def4", "#524f67", "#403d52"],
    "#26233a #eb6f92 #31748f #f6c177 #9ccfd8 #c4a7e7 #ebbcba #e0def4 #6e6a86 #eb6f92 #31748f #f6c177 #9ccfd8 #c4a7e7 #ebbcba #e0def4",
  ),
  theme(
    "nord",
    "Nord",
    "dark",
    ["#2e3440", "#d8dee9", "#d8dee9", "#434c5e"],
    "#3b4252 #bf616a #a3be8c #ebcb8b #81a1c1 #b48ead #88c0d0 #e5e9f0 #4c566a #bf616a #a3be8c #ebcb8b #81a1c1 #b48ead #8fbcbb #eceff4",
  ),
  theme(
    "gruvbox-dark",
    "Gruvbox Dark",
    "dark",
    ["#282828", "#ebdbb2", "#ebdbb2", "#504945"],
    "#282828 #cc241d #98971a #d79921 #458588 #b16286 #689d6a #a89984 #928374 #fb4934 #b8bb26 #fabd2f #83a598 #d3869b #8ec07c #ebdbb2",
  ),
  theme(
    "gruvbox-light",
    "Gruvbox Light",
    "light",
    ["#fbf1c7", "#3c3836", "#3c3836", "#d5c4a1"],
    "#fbf1c7 #cc241d #98971a #d79921 #458588 #b16286 #689d6a #7c6f64 #928374 #9d0006 #79740e #b57614 #076678 #8f3f71 #427b58 #3c3836",
  ),
  theme(
    "one-dark",
    "One Dark",
    "dark",
    ["#282c34", "#abb2bf", "#528bff", "#3e4451"],
    "#282c34 #e06c75 #98c379 #e5c07b #61afef #c678dd #56b6c2 #abb2bf #5c6370 #e06c75 #98c379 #e5c07b #61afef #c678dd #56b6c2 #ffffff",
  ),
  theme(
    "solarized-dark",
    "Solarized Dark",
    "dark",
    ["#002b36", "#839496", "#93a1a1", "#073642"],
    "#073642 #dc322f #859900 #b58900 #268bd2 #d33682 #2aa198 #eee8d5 #002b36 #cb4b16 #586e75 #657b83 #839496 #6c71c4 #93a1a1 #fdf6e3",
  ),
  theme(
    "solarized-light",
    "Solarized Light",
    "light",
    ["#fdf6e3", "#657b83", "#586e75", "#eee8d5"],
    "#073642 #dc322f #859900 #b58900 #268bd2 #d33682 #2aa198 #eee8d5 #002b36 #cb4b16 #586e75 #657b83 #839496 #6c71c4 #93a1a1 #fdf6e3",
  ),
  theme(
    "github-dark",
    "GitHub Dark",
    "dark",
    ["#0d1117", "#e6edf3", "#e6edf3", "#264f78"],
    "#484f58 #ff7b72 #3fb950 #d29922 #58a6ff #bc8cff #39c5cf #b1bac4 #6e7681 #ffa198 #56d364 #e3b341 #79c0ff #d2a8ff #56d4dd #ffffff",
  ),
  theme(
    "github-light",
    "GitHub Light",
    "light",
    ["#ffffff", "#1f2328", "#1f2328", "#b6e3ff"],
    "#24292f #cf222e #116329 #4d2d00 #0969da #8250df #1b7c83 #6e7781 #57606a #a40e26 #1a7f37 #633c01 #218bff #a475f9 #3192aa #8c959f",
  ),
  theme(
    "monokai",
    "Monokai",
    "dark",
    ["#272822", "#f8f8f2", "#f8f8f0", "#49483e"],
    "#272822 #f92672 #a6e22e #f4bf75 #66d9ef #ae81ff #a1efe4 #f8f8f2 #75715e #f92672 #a6e22e #f4bf75 #66d9ef #ae81ff #a1efe4 #f9f8f5",
  ),
]

/**
 * The chosen theme, or, until someone picks one, Paperboat Dark or Light to suit the console's
 * appearance.
 */
export function resolveTerminalTheme(id: string | null, appAppearance: "dark" | "light"): TerminalTheme {
  return (
    terminalThemes.find((entry) => entry.id === id) ??
    terminalThemes.find((entry) => entry.id === `paperboat-${appAppearance}`)!
  )
}

/**
 * Ghostty's core resolves the 16 ANSI colours itself, from its built-in palette, and hands the
 * renderer finished RGB values, so the CSS variables alone don't recolour them. These standard
 * escape sequences (OSC 4 for the palette, 10/11/12 for foreground, background and cursor) set
 * the theme inside the core. They are written to the local renderer only, never to the machine.
 */
export function paletteSequence(theme: TerminalTheme): string {
  const rgb = (hex: string) => `rgb:${hex.slice(1, 3)}/${hex.slice(3, 5)}/${hex.slice(5, 7)}`
  const st = "\x1b\\"
  return (
    theme.ansi.map((color, index) => `\x1b]4;${index};${rgb(color)}${st}`).join("") +
    `\x1b]10;${rgb(theme.foreground)}${st}\x1b]11;${rgb(theme.background)}${st}\x1b]12;${rgb(theme.cursor)}${st}`
  )
}

/** The CSS variables wterm reads, for the terminal's host element. */
export function terminalThemeStyle(theme: TerminalTheme): Record<string, string> {
  const style: Record<string, string> = {
    "--term-bg": theme.background,
    "--term-fg": theme.foreground,
    "--term-cursor": theme.cursor,
    "--term-selection-bg": theme.selection,
  }
  theme.ansi.forEach((color, index) => (style[`--term-color-${index}`] = color))
  return style
}
