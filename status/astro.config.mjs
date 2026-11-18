import { defineConfig } from "astro/config"

// Static output. The page imports the shared brand package (../brand: tokens, fonts, base styles, the mark and lockup, favicons); nothing is copied.

export default defineConfig({
  site: process.env.PAPERBOAT_STATUS_URL || "http://localhost:4321",
  trailingSlash: "always",
  devToolbar: { enabled: false },
  compressHTML: true,
  build: { inlineStylesheets: "always" },
  vite: {
    server: { fs: { allow: [".."] } },
  },
})
