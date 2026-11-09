// Checks the built site (dist/) without a browser: `npm run build && npm run check:site`.
// It fails on anything a visitor or a search engine would trip over: a missing or repeated title,
// a broken internal link or anchor, placeholder text, a page without a real command, a claim that
// does not match the server's configuration, a missing share image, invalid structured data.
import { readFileSync, readdirSync, existsSync, statSync } from "node:fs"
import { join } from "node:path"

const dist = new URL("../dist/", import.meta.url).pathname
const env = Object.fromEntries(
  readFileSync(new URL("../.env", import.meta.url), "utf8")
    .split("\n")
    .filter((line) => line.includes("="))
    .map((line) => [line.slice(0, line.indexOf("=")), line.slice(line.indexOf("=") + 1).trim()]),
)
const config = (await (await fetch(new URL("/v1/client-configuration", env.PAPERBOAT_API_URL))).json()).data

const pages = new Map() // "/pricing/" -> html
const walk = (dir) => {
  for (const name of readdirSync(dir)) {
    const path = join(dir, name)
    if (statSync(path).isDirectory()) walk(path)
    else if (name === "index.html") pages.set("/" + path.slice(dist.length).replace(/index\.html$/, ""), readFileSync(path, "utf8"))
    else if (name === "404.html") pages.set("/404.html", readFileSync(path, "utf8"))
  }
}
walk(dist)

const failures = []
const fail = (page, message) => failures.push(`${page}: ${message}`)
const text = (html) => html.replace(/<(script|style)[\s\S]*?<\/\1>/g, " ").replace(/<[^>]+>/g, " ").replace(/&[a-z#0-9]+;/g, " ")
const first = (html, pattern) => html.match(pattern)?.[1]
const titles = new Map()
const descriptions = new Map()
const sitemap = readFileSync(join(dist, "sitemap.xml"), "utf8")

for (const [path, html] of pages) {
  const body = text(html)
  const noindex = /<meta name="robots" content="noindex"/.test(html)

  // One title, one description, one h1; unique across the site.
  const title = first(html, /<title>([^<]*)<\/title>/)
  const description = first(html, /<meta name="description" content="([^"]*)"/)
  if (!title || title.length < 20 || title.length > 80) fail(path, `title length ${title?.length ?? 0} (want 20-80): ${title}`)
  if (!description || description.length < 70 || description.length > 260) fail(path, `description length ${description?.length ?? 0} (want 70-260)`)
  if (titles.has(title)) fail(path, `same title as ${titles.get(title)}`)
  if (descriptions.has(description)) fail(path, `same description as ${descriptions.get(description)}`)
  titles.set(title, path)
  descriptions.set(description, path)
  if ((html.match(/<h1[\s>]/g) ?? []).length !== 1) fail(path, "needs exactly one h1")

  // Canonical, share image and sitemap agree with whether the page is indexable.
  const canonical = first(html, /<link rel="canonical" href="([^"]*)"/)
  if (!noindex && canonical !== new URL(path, env.PAPERBOAT_SITE_URL).href) fail(path, `canonical is ${canonical}`)
  if (!noindex && !sitemap.includes(`<loc>${canonical}</loc>`)) fail(path, "missing from sitemap.xml")
  if (noindex && sitemap.includes(new URL(path, env.PAPERBOAT_SITE_URL).href + "<")) fail(path, "noindex page is in sitemap.xml")
  const image = first(html, /<meta property="og:image" content="([^"]*)"/)
  if (!image || !existsSync(join(dist, new URL(image).pathname))) fail(path, `share image missing: ${image}`)

  // Structured data parses.
  for (const [, json] of html.matchAll(/<script type="application\/ld\+json">([\s\S]*?)<\/script>/g)) {
    try {
      JSON.parse(json)
    } catch {
      fail(path, "invalid JSON-LD")
    }
  }

  // Internal links and anchors resolve.
  for (const [, href] of html.matchAll(/<a [^>]*href="([^"]+)"/g)) {
    if (!href.startsWith("/") && !href.startsWith("#")) continue
    const [target, anchor] = href.split("#")
    const page = target === "" ? path : target.split("?")[0]
    const found = pages.get(page)
    if (!found) fail(path, `broken link ${href}`)
    else if (anchor && !new RegExp(`id="${anchor}"`).test(found)) fail(path, `broken anchor ${href}`)
  }
  for (const [, tag] of html.matchAll(/(<img [^>]*>)/g)) if (!/ alt="/.test(tag)) fail(path, `image without alt: ${tag.slice(0, 80)}`)

  // No placeholder or filler copy; no Windows claim beyond "coming"; no removed domains.
  for (const banned of [/being written/i, /keep going/i, /coming soon/i, /lorem ipsum/i, /\bseamless/i, /\bsupercharge/i, /\bunlock\b/i, /\brobust\b/i, /in today.s world/i, /—/])
    if (banned.test(body)) fail(path, `banned text ${banned}`)
  // Windows is not shipped: our own pages may only say it is coming. (Compare pages list other products' platforms.)
  if (!path.startsWith("/compare/"))
    for (const [hit] of body.matchAll(/.{0,40}\bWindows\b(?! (support )?is coming| coming).{0,40}/g)) fail(path, `Windows mentioned as supported: "${hit.trim()}"`)
  if (/powershell/i.test(body)) fail(path, "PowerShell mentioned")
  if (/\.local\.pprbt\.dev|preview\.pprbt\.dev|tunnels\.pprbt\.dev/.test(html)) fail(path, "removed domain")
}

// Every feature and use-case page shows a real command.
for (const [path, html] of pages)
  if (/^\/(features|use-cases)\/[^/]+\/$/.test(path) && !/class="[^"]*\bsheet\b/.test(html)) fail(path, "no command example")

// Claims that must match the server's configuration and the plan table (PLAN 1.3).
const home = pages.get("/")
const pricing = text(pages.get("/pricing/"))
if (!home.includes(`data-copy="curl -fsSL ${config.installer_base_url}/install | bash"`)) fail("/", "install command does not use the configured installer host")
if (!home.includes(`href="${config.console_url}"`)) fail("/", "sign-in link is not the configured console")
if (!text(home).includes(`.${config.preview_base_domain}`) || !text(home).includes(`.${config.local_access_base_domain}`)) fail("/", "example addresses are not the configured domains")
for (const claim of ["$0", "$8", "$80/year", "$15", "$150/seat/year", "5 GB", "100 GB", "1,000", "Unlimited", "14-day", "$5 per 100 GB", "30 days", "90 days", "365 days"])
  if (!pricing.includes(claim)) fail("/pricing/", `missing pricing claim ${claim}`)
if (!existsSync(join(dist, "robots.txt"))) fail("/robots.txt", "missing")

if (failures.length) {
  console.error(failures.join("\n"))
  console.error(`\nsite-check: ${failures.length} problem(s) in ${pages.size} pages`)
  process.exit(1)
}
console.log(`site-check: ${pages.size} pages, 0 problems`)
