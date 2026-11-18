import { defineConfig } from "astro/config"
import { fileURLToPath } from "node:url"

// Static output. The page reuses the marketing site's brand styles, fonts and mark by importing them from ../marketing (nothing is copied);
// a CSS @import there resolves packages from marketing/, which is not installed here, so the three font packages point at this project's.
const font = (name) => fileURLToPath(new URL(`./node_modules/@fontsource-variable/${name}/index.css`, import.meta.url))

export default defineConfig({
  site: process.env.PAPERBOAT_STATUS_URL || "http://localhost:4321",
  trailingSlash: "always",
  devToolbar: { enabled: false },
  compressHTML: true,
  build: { inlineStylesheets: "always" },
  vite: {
    resolve: { alias: { "@fontsource-variable/figtree": font("figtree"), "@fontsource-variable/geist": font("geist"), "@fontsource-variable/jetbrains-mono": font("jetbrains-mono") } },
    server: { fs: { allow: [".."] } },
  },
})
