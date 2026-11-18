import { defineConfig } from "astro/config"

// Static output. The page reuses the marketing site's brand styles, fonts and mark by importing them from ../marketing (nothing is copied);
// the build installs marketing's dependencies too (deploy.sh sets the Pages build command), which is where the font packages resolve from.

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
