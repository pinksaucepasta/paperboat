// Checks the built site (dist/) without a browser: `bun run build && bun run check:site`.
// It fails on anything a visitor or a search engine would trip over: a missing or repeated title,
// a broken internal link or anchor, placeholder text, a page without a real command, a claim that
// does not match the server's configuration, a missing share image, invalid structured data.
import { readFileSync, readdirSync, existsSync, statSync } from "node:fs"
import { join } from "node:path"
import { spawnSync } from "node:child_process"
import { checkCommands } from "./commands.mjs"

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

  // No word run into a link or a command ("aside.What our servers"): a space was lost in the markup.
  const marked = html.replace(/<(script|style)[\s\S]*?<\/\1>/g, "")
  for (const [hit] of marked.matchAll(/[A-Za-z,.;:]<(?:a|code|strong|kbd)\b(?![^>]*class="(?:fn|command)")[^>]*>[^<]{0,24}|<\/(?:a|code|strong|kbd)>[A-Za-z][^<]{0,24}/g))
    fail(path, `missing space around inline element: ${hit.replace(/<[^>]*>/g, "|")}`)

  // (Never call our own text honest or plain; "in seconds" and "in a minute" are time claims nobody measured.)
  // No placeholder or filler copy; no removed domains.
  for (const banned of [/being written/i, /keep going/i, /coming soon/i, /lorem ipsum/i, /\bseamless/i, /\bsupercharge/i, /\bunlock\b/i, /\brobust\b/i, /in today.s world/i, /—/, /\bhonest/i, /plain account/i, /\bin seconds\b/i, /\b(with)?in a minute\b/i, /\bhappily\b/i])
    if (banned.test(body)) fail(path, `banned text ${banned}`)
  if (/\.local\.pprbt\.dev|preview\.pprbt\.dev|tunnels\.pprbt\.dev/.test(html)) fail(path, "removed domain")
}

// Every feature and use-case page shows a real command.
for (const [path, html] of pages)
  if (/^\/(features|use-cases)\/[^/]+\/$/.test(path) && !/class="[^"]*\bsheet\b/.test(html)) fail(path, "no command example")

// Claims that must match the server's configuration and the plan table (PLAN 1.3).
const home = pages.get("/")
const pricing = text(pages.get("/pricing/")).replace(/\s+/g, " ")
if (!home.includes(`data-copy="curl -fsSL ${config.installer_base_url}/install | bash"`)) fail("/", "install command does not use the configured installer host")
if (!home.includes(`data-copy="irm ${config.installer_base_url}/install.ps1 | iex"`)) fail("/", "Windows install command does not use the configured installer host")
if (!home.includes(`href="${config.console_url}"`)) fail("/", "sign-in link is not the configured console")
if (!text(home).includes(`.${config.preview_base_domain}`)) fail("/", "the example preview address is not on the configured domain")
if (!text(pages.get("/features/private-access/")).includes(`.${config.local_access_base_domain}`)) fail("/features/private-access/", "example addresses are not on the configured local domain")
for (const claim of ["$0", "$8", "$80 a year", "$15", "$150 a seat a year", "5 GB", "100 GB", "1,000", "Unlimited", "14-day", "$5 per 100 GB", "30 days", "90 days", "365 days"])
  if (!pricing.includes(claim)) fail("/pricing/", `missing pricing claim ${claim}`)
if (!existsSync(join(dist, "robots.txt"))) fail("/robots.txt", "missing")

// Every pb and pbh command on the site runs as written (checks/commands.mjs). Inside the repository this
// needs a pb built from the same checkout: PB_BIN=/path/to/pb (cd cli && go build -o /path/to/pb ./cmd/pb).
const repo = new URL("../../", import.meta.url).pathname
if (existsSync(join(repo, "cli/cmd/pb"))) {
  if (!process.env.PB_BIN || !existsSync(process.env.PB_BIN)) fail("commands", "set PB_BIN to a pb built from this checkout (cd cli && go build -o /tmp/pb ./cmd/pb); the commands on the site are checked against it")
  else console.log(`site-check: ${checkCommands(pages, fail, { pb: process.env.PB_BIN, repo })} commands checked against ${process.env.PB_BIN}`)
}

// Numbers that have a source in the code are compared with it, when this build runs inside the repository.
//   Checked here: machine, preview/tunnel, browser-terminal and public-TCP limits, the free bandwidth
//   allowance, the trial length, shared-terminal viewers (server/internal/config/config.go); audit log
//   retention and the SSO seat minimum (config/business.go); the top-up size (billing/workspace_topups.go);
//   the file-send limits (config.go).
//   NOT checked, because no code holds them (they live in Stripe or the database): the prices ($8, $80,
//   $15, $150), the 100 GB of Pro and Team, the top-up price ($5), the 7-day grace after a failed payment.
//   Those follow PLAN.md 1.3 and are only asserted to be on the page.
const source = (path) => (existsSync(join(repo, path)) ? readFileSync(join(repo, path), "utf8") : "")
const serverConfig = source("server/internal/config/config.go")
if (serverConfig) {
  const business = source("server/internal/config/business.go")
  const number = (from, pattern) => Number(from.match(pattern)?.[1]?.replace(/_/g, ""))
  const shown = (n) => n.toLocaleString("en-US")
  const pageText = (page) => text(pages.get(page)).replace(/\s+/g, " ")
  const expect = (page, wanted, what) => {
    if (wanted.includes("NaN") || !pageText(page).includes(wanted)) fail(page, `${what} differs from the server's (expected "${wanted}")`)
  }
  const limit = (name) => [number(serverConfig, new RegExp(`${name}: \\w+\\{Free: (\\d+)`)), number(serverConfig, new RegExp(`${name}: \\w+\\{Free: \\d+, Pro: (\\d+)`))]
  for (const [row, name] of [["Machines", "Machines"], ["Previews and tunnels running at once", "Publications"], ["Terminals open in the web console at once", "BrowserTerminals"]]) {
    const [free, pro] = limit(name)
    expect("/pricing/", `${row} ${shown(free)} ${shown(pro)} Unlimited`, `${name} limits`)
  }
  const [tcpFree, tcpPro] = limit("PublicTCP")
  expect("/pricing/", `Public raw-TCP tunnel addresses ${tcpFree} ${tcpPro} ${number(serverConfig, /TeamPerSeat: (\d+)/)} per seat`, "public TCP limits")
  expect("/pricing/", `${number(serverConfig, /TeamTrialDays: (\d+)/)}-day`, "Team trial length")
  expect("/pricing/", `${number(serverConfig, /PersonalAllowanceBytes: new\(int64\(([\d_]+)/) / 1e9} GB`, "free bandwidth")
  expect("/pricing/", `${number(serverConfig, /ViewersPerSharedTerminal: (\d+)/)} possible viewers`, "shared terminal viewers")
  const days = (plan) => number(business, new RegExp(`AuditRetention${plan}: +(\\d+) \\* day`))
  expect("/pricing/", `Audit log kept for ${days("Free")} days ${days("Pro")} days ${days("Team")} days`, "audit log retention")
  expect("/pricing/", `From ${number(business, /SSOMinSeats: (\d+)/)} seats`, "SSO seat minimum")
  expect("/pricing/", `per ${number(source("server/internal/billing/workspace_topups.go"), /TopupUnitBytes int64 = ([\d_]+)/) / 1e9} GB`, "top-up size")
  expect("/features/files/", `Up to ${number(serverConfig, /AutomaticMaxBatchBytes: ([\d_]+)/) / 1e9} GB per send`, "file size limit")
  expect("/features/files/", `Files per send Up to ${number(serverConfig, /MaxBatchFiles: (\d+)/)}.`, "files per send")
}

// One type scale: every font size in the source is a token (the lockup follows the brand's mark-height rule).
const sources = (dir) => readdirSync(dir, { withFileTypes: true }).flatMap((entry) => (entry.isDirectory() ? sources(join(dir, entry.name)) : [join(dir, entry.name)]))
for (const file of sources(new URL("../src/", import.meta.url).pathname).filter((name) => /\.(astro|css)$/.test(name) && !name.endsWith("Lockup.astro")))
  for (const [declaration] of readFileSync(file, "utf8").matchAll(/font-size:[^;]+;/g))
    if (!/var\(--text-|inherit|0\.86em/.test(declaration) && !/--text-/.test(declaration)) fail(file.split("/src/")[1], `font size outside the token set: ${declaration}`)

// With --external, every link to another site is fetched too (the sources of the comparison pages).
// A site that refuses automated requests (401, 403, 429) is listed but does not fail the check.
if (process.argv.includes("--external")) {
  const links = new Set()
  for (const html of pages.values()) for (const [, href] of html.matchAll(/<a [^>]*href="(https:\/\/[^"]+)"/g)) links.add(href)
  const refused = []
  await Promise.all(
    [...links].map(async (href) => {
      try {
        const response = await fetch(href, { redirect: "follow", signal: AbortSignal.timeout(20_000), headers: { "User-Agent": "Mozilla/5.0 (link check)" } })
        if ([401, 403, 429].includes(response.status)) refused.push(`${response.status} ${href}`)
        else if (!response.ok) fail(href, `external link answers ${response.status}`)
      } catch (error) {
        // A few servers drop Node's connections but answer curl; ask once more that way before calling the link dead.
        const code = spawnSync("curl", ["-sS", "-L", "-o", "/dev/null", "-m", "25", "-w", "%{http_code}", href]).stdout?.toString()
        if (!/^[23]/.test(code ?? "")) fail(href, `external link failed: ${error.cause?.code ?? error.name}`)
      }
    }),
  )
  console.log(`site-check: ${links.size} external links, ${refused.length} refused automated requests${refused.length ? ":\n  " + refused.join("\n  ") : ""}`)
}

if (failures.length) {
  console.error(failures.join("\n"))
  console.error(`\nsite-check: ${failures.length} problem(s) in ${pages.size} pages`)
  process.exit(1)
}
console.log(`site-check: ${pages.size} pages, 0 problems`)
