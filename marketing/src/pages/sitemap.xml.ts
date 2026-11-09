/** Every page meant to be found. Legal drafts and the 404 page are left out (they are noindex). */
import type { APIRoute } from "astro"
import { features, useCases } from "../features"
import { compares } from "../compare-data"

export const paths = [
  "/",
  "/features/",
  ...features.map((item) => `/features/${item.slug}/`),
  ...useCases.map((item) => `/use-cases/${item.slug}/`),
  "/compare/",
  ...compares.map((item) => `/compare/${item.slug}/`),
  "/pricing/",
  "/security/",
  "/docs/",
]

export const GET: APIRoute = ({ site }) => {
  const urls = paths.map((path) => `<url><loc>${new URL(path, site).href}</loc></url>`).join("")
  return new Response(`<?xml version="1.0" encoding="UTF-8"?><urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">${urls}</urlset>`, {
    headers: { "Content-Type": "application/xml" },
  })
}
