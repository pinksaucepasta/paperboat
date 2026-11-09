import { defineConfig } from "astro/config"
import { loadEnv } from "vite"

// The site's own address (canonical links, the sitemap, share images) is configuration, like the API address.
// A build needs it; the dev server falls back to its own local address.
const { PAPERBOAT_SITE_URL } = loadEnv(process.env.NODE_ENV ?? "production", process.cwd(), "PAPERBOAT_")
if (!PAPERBOAT_SITE_URL && !process.argv.includes("dev")) throw new Error("PAPERBOAT_SITE_URL is required (see marketing/.env.example)")

export default defineConfig({
  site: PAPERBOAT_SITE_URL ?? "http://localhost:4321",
  trailingSlash: "always",
  devToolbar: { enabled: false },
  // Lossless whitespace handling. Astro 7's default ("jsx") removes the line break between a line of text and
  // a link that starts the next line, which glues the two words together.
  compressHTML: true,
  build: { inlineStylesheets: "always" },
})
