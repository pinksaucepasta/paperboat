// Browser check of the public-traffic panel on the usage page and the banner on every page. It runs the console from this checkout
// against a small stub control plane (no real server, nothing is sent or changed) and walks four workspace states:
//   under (40% used), nearly (85%), throttled (past the amount, 1 Mbit/s until the period ends) and own (runs its own edges: no limits).
// usage: node scripts/check-fair-use.mjs            PW_MODULE=<playwright-core index.js> PW_CHROMIUM=<chrome path> when not installed
// It starts `next dev` on a free port with PAPERBOAT_SERVER_URL pointing at the stub. Exits 1 when a step fails.
import { createServer } from "node:http"
import { spawn } from "node:child_process"
import { createServer as net } from "node:net"

const playwright = await import(process.env.PW_MODULE ?? "playwright-core")
const chromium = playwright.chromium ?? playwright.default.chromium
const freePort = () => new Promise((resolve) => { const s = net().listen(0, "127.0.0.1", () => { const { port } = s.address(); s.close(() => resolve(port)) }) })

const GB = 1e9
let state = "under"
// The console rejects any answer that does not follow the OpenAPI shapes, so every stubbed route does.
// One period for the whole run: the usage query is keyed by its start, so a start that moved on every answer would never settle.
const PERIOD_START = new Date(Date.now() - 21 * 86400_000).toISOString()
const PERIOD_END = new Date(Date.now() + 9 * 86400_000).toISOString()
const PAGE = { limit: 200, offset: 0, total: 0, next_offset: null }
const USED = { under: 40 * GB, nearly: 85 * GB, throttled: 104 * GB, own: 3 * GB, team: 150 * GB }
const billing = () => {
  const used = USED[state]
  return {
    id: "personal:usr_1", state: "active", seats_bought: 1, members: 1, can_manage: true, cancel_at_period_end: false, seats_used: 1,
    usage_bytes: used, allowance_bytes: state === "team" ? 400 * GB : 100 * GB,
    ...(state === "team" ? { allowance_seats: 4, allowance_per_seat_bytes: 100 * GB } : {}), allowance_period_start: PERIOD_START, allowance_period_end: PERIOD_END,
    relay_mbit: 100, sso_addon: false, scim_addon: false,
    topup_bytes_remaining: 0, topup_bytes_purchased: 0, topups: [], granted: false, throttled: state === "throttled", throttle_lifted: false, throttle_mbit: 1,
    limits: state === "own"
      ? { machines: { used: 1, limit: 100 }, publications: { used: 0, limit: null, self_hosted: true }, browser_terminals: { used: 0, limit: 10 }, public_tcp: { used: 0, limit: null, self_hosted: true } }
      : { machines: { used: 1, limit: 100 }, publications: { used: 0, limit: 100 }, browser_terminals: { used: 0, limit: 10 }, public_tcp: { used: 0, limit: 1 } },
  }
}
const usage = () => ({
  start: new Date(Date.now() - 86400_000).toISOString(), end: new Date().toISOString(), updated_at: new Date().toISOString(),
  coverage: { edge: true, native: true, note: "" },
  // Hosted traffic 0.6 relay + 0.4 edge of what the pool says is used (see `billing`), plus a self-hosted path that must not show.
  totals: {
    upload_bytes: 0, download_bytes: 0,
    by_path: [
      { mode: "relay", hosting: "managed", upload_bytes: Math.round(0.3 * USED[state]), download_bytes: Math.round(0.3 * USED[state]) },
      { mode: "edge", hosting: "managed", upload_bytes: Math.round(0.2 * USED[state]), download_bytes: Math.round(0.2 * USED[state]) },
      { mode: "relay", hosting: "self_hosted", upload_bytes: 7 * GB, download_bytes: 0 },
    ],
  }, resources: [], series: [],
  pagination: PAGE,
})
const api = createServer((request, response) => {
  const path = new URL(request.url, "http://x").pathname.replace(/^\/v1\//, "")
  const answers = {
    me: { id: "usr_1", email: "ana@example.test", display_name: "Ana", status: "active", role: "user", workos_subject: "sub_1" },
    workspaces: { items: [{ id: "personal", name: "Personal", kind: "personal", role: "owner" }] },
    "billing/workspaces/personal": billing(),
    usage: usage(),
    machines: { items: [], pagination: PAGE, total: 0, next_cursor: "" },
    "machines/summary": { total: 0, by_state: { online: 0, offline: 0, pending: 0 }, by_platform: { linux: 0, macos: 0, windows: 0 } },
    "team-inbox/requests": { requests: [], pagination: PAGE },
    "team-invitations": { items: [], pagination: PAGE },
    announcements: { banner: null, notices: [], unread: 0 },
  }
  const data = answers[path]
  response.writeHead(data ? 200 : 404, { "content-type": "application/json", "cache-control": "no-store" })
  response.end(JSON.stringify(data ? { data } : { error: { code: "not_found", message: "stub" } }))
})
await new Promise((resolve) => api.listen(0, "127.0.0.1", resolve))
const apiPort = api.address().port
const port = await freePort()
const next = spawn("bunx", ["next", "dev", "-p", String(port)], {
  env: { ...process.env, PAPERBOAT_SERVER_URL: `http://127.0.0.1:${apiPort}`, WORKOS_REDIRECT_URI: `http://127.0.0.1:${port}/auth/callback`, NEXT_TELEMETRY_DISABLED: "1" },
  stdio: "ignore",
})
let failed = 0
const check = (step, ok, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
let browser
try {
  const origin = `http://127.0.0.1:${port}`
  for (let i = 0; i < 120; i++) { if (await fetch(origin + "/auth/sign-in").then(() => true, () => false)) break; await new Promise((r) => setTimeout(r, 1000)) }
  browser = await chromium.launch(process.env.PW_CHROMIUM ? { executablePath: process.env.PW_CHROMIUM } : {})
  for (const [name, width] of [["desktop", 1440], ["phone", 390]]) {
    for (const s of ["under", "nearly", "throttled", "own", "team"]) {
      state = s
      const context = await browser.newContext({ viewport: { width, height: 900 } })
      await context.addCookies([{ name: "pb-dev-session", value: "stub", url: origin }])
      const page = await context.newPage()
      const problems = []
      page.on("pageerror", (e) => problems.push(`pageerror: ${e.message}`))
      const trace = []
      const t0 = Date.now()
      page.on("console", (m) => m.type() === "error" && trace.push(`console ${m.text().slice(0, 120)}`))
      page.on("response", (r) => /\/usage\b/.test(r.url()) && trace.push(`${Date.now() - t0}ms ${r.status()} ${new URL(r.url()).search.slice(0, 50)}`))
      page.on("response", async (r) => r.status() >= 400 && problems.push(`${r.status()} ${new URL(r.url()).pathname} ${(await r.text().catch(() => "")).slice(0, 160)}`))
      await page.goto(`${origin}/usage`, { timeout: 120_000 })
      const panel = await page.waitForSelector("[data-fair-use]", { timeout: 60_000 }).catch(() => null)
      // Always query the live panel: React may replace the section, and a handle to a replaced one keeps the text of its first render.
      const live = page.locator("[data-fair-use]")
      check(`${name}/${s}: the usage page shows the public-traffic panel`, Boolean(panel), panel ? "" : `${(await page.innerText("body").catch(() => "")).replace(/\s+/g, " ").slice(0, 200)} | ${problems.slice(0, 8).join(" | ")}`)
      if (!panel) { failed++; await context.close(); continue }
      let waitError = ""
      const homeCard = async (viewName, state) => {
        const home = await context.newPage()
        await home.goto(`${origin}/`, { timeout: 120_000 })
        const el = await home.waitForSelector("[data-fair-use-card]", { timeout: 60_000 }).catch(() => null)
        const liveCard = home.locator("[data-fair-use-card]")
        if (!el) { await home.close(); return { title: "", text: "", clipped: true } }
        // Selector waits (a page function may be blocked by the app's CSP); a wait that fails is reported, never skipped.
        waitError = await home.locator('[data-fair-use-card] [title^="Relay"]').first().waitFor({ timeout: 60_000 }).then(() => home.locator("[data-fair-use-card] [class*=animate-pulse]").first().waitFor({ state: "detached", timeout: 30_000 })).then(() => "", (e) => String(e.message).split("\n")[0].slice(0, 100))
        const out = await liveCard.evaluate((node) => {
          const card = node.parentElement
          const clipped = [...card.querySelectorAll("*")].some((e) => e.scrollWidth > e.clientWidth + 1 && getComputedStyle(e).overflow !== "visible" && e.clientWidth > 0)
          const leaves = []
          for (const walker = document.createTreeWalker(card, NodeFilter.SHOW_TEXT); walker.nextNode();) leaves.push(walker.currentNode.textContent.trim())
          return { title: card.firstElementChild.innerText.trim(), text: leaves.filter(Boolean).join(" "), clipped }
        })
        if (process.env.SHOTS) await home.screenshot({ path: `${process.env.SHOTS}/home-${viewName}-${state}.png` })
        await home.close()
        return out
      }
      const panelWait = s === "own" ? "" : await page.locator('[data-fair-use] [title^="Relay"]').first().waitFor({ timeout: 60_000 }).then(() => "", (e) => String(e.message).split("\n")[0].slice(0, 100))
      const text = (await live.innerText()).replace(/\s+/g, " ")
      // The same text with a space between every element's text (innerText drops it between flex items): headline, rows and legend.
      const spaced = await live.evaluate((node) => {
        const leaves = []
        for (const walker = document.createTreeWalker(node, NodeFilter.SHOW_TEXT); walker.nextNode();) leaves.push(walker.currentNode.textContent.trim())
        return leaves.filter(Boolean).join(" ")
      })
      const text0 = text
      check(`${name}/${s}: no asterisks`, !/\*/.test(text0))
      if (s === "team") {
        check(`${name}/${s}: a team's included traffic is per seat, pooled: 100 GB × 4 seats = 400 GB`, /400 GB a month of traffic included \(100 GB × 4 seats, shared by the team\)/.test(text) && /150 GB of 400 GB/.test(spaced), text.slice(0, 160))
      } else if (s === "own") {
        check(`${name}/${s}: it says Unlimited (your own edges) and that nothing is counted`, /Unlimited \(your own edges\)/.test(text) && /never counted or limited by Paperboat/.test(text), text.slice(0, 90))
        check(`${name}/${s}: no upsell, no banner`, (await live.locator("a").count()) === 0 && !(await page.$("[data-quota-banner]")))
      } else {
        const used = { under: 40, nearly: 85, throttled: 104 }[s]
        const re = (gb) => `${Math.floor(gb + 1e-6)}\\.?\\d* GB`
        check(`${name}/${s}: it shows what is used of the amount`, new RegExp(`${re(used)} of 100 GB`).test(spaced), spaced.slice(0, 90))
        check(`${name}/${s}: it says plainly what happens after: reduced speed, billing date, never cut off or billed extra`, /100 GB a month of traffic included/.test(text) && /Edges and relays share it: previews, tunnels and relayed connections count\. Files you upload through a console terminal count too; typing and terminal output do not, and are never slowed by it/.test(text) && /speed is reduced to 1 Mbit\/s until your next billing date\. Never cut off, never billed extra\./.test(text) && /Until then relayed connections run at up to 100 Mbit\/s and edges at full speed\./.test(text) && /Run your own edge or relay \(free, open source\): no limits\./.test(text))
        // Hosted allowance: "<X> left" (or the slowed state in its place), a bar split relay/edge, "<used> of <total>", "Resets <date>", legend.
        const legend = new RegExp(`Relay ${re(used * 0.6)} Edge ${re(used * 0.4)} `)
        check(`${name}/${s}: legend shows relay and edge hosted traffic only (self-hosted never counts)`, legend.test(spaced) && !/\b7\.?\d* GB/.test(spaced), spaced.slice(0, 120))
        const bars = await live.locator("[role=meter] > div").evaluateAll((d) => d.map((e) => ({ w: parseFloat(e.style.width), c: e.className })))
        check(`${name}/${s}: the bar is split relay/edge 60/40 of the used share`, bars.length === 2 && /relay/.test(bars[0].c) && /edge/.test(bars[1].c) && Math.abs(bars[0].w / (bars[0].w + bars[1].w) - 0.6) < 0.01 && Math.abs(bars[0].w + bars[1].w - Math.min(100, used)) < 0.5, `${JSON.stringify(bars)} | wait: ${panelWait || "ok"} | ${trace.join(" | ")}`)
        check(`${name}/${s}: it says when the period resets`, /Resets [A-Za-z0-9 ]+/.test(text))
        const card = await homeCard(name, s)
        check(`${name}/${s}: home card is titled Hosted allowance`, card.title === "Hosted allowance", card.title)
        check(`${name}/${s}: home card shows <used> of <total>, Resets <date>, legend, nothing truncated`, new RegExp(`${re(used)} of 100 GB Resets .+ Relay ${re(used * 0.6)} Edge ${re(used * 0.4)}$`).test(card.text) && !card.clipped, `${card.text}${card.clipped ? " (clipped)" : ""} | wait: ${waitError || "ok"}`)
        if (s === "throttled") {
          check(`${name}/${s}: it says slowed to 1 Mbit/s until the reset date`, /Slowed to 1 Mbit\/s until/.test(spaced) && /Slowed to 1 Mbit\/s until/.test(card.text) && !/left/.test(card.text), `${spaced.slice(0, 40)} | ${card.text.slice(0, 40)}`)
          const links = (await live.locator("a").evaluateAll((a) => a.map((e) => `${e.textContent.trim()}:${e.getAttribute("href")}`))).join(" ")
          check(`${name}/${s}: it offers add bandwidth and change plan`, /Add bandwidth:\/billing/.test(links) && /Change plan:\/billing/.test(links), links)
          const banner = await page.waitForSelector("[data-quota-banner]", { timeout: 30_000 }).catch(() => null)
          const bannerText = ((await banner?.innerText()) ?? "").replace(/\s+/g, " ")
          check(`${name}/${s}: the banner is calm: slowed, never cut off`, /Slowed to 1 Mbit\/s until/.test(bannerText) && /Never cut off, never billed extra/.test(bannerText) && !/paused|stopped|exhausted|\*/i.test(bannerText), bannerText.slice(0, 80))
        } else {
          const left = { under: 60, nearly: 15 }[s]
          check(`${name}/${s}: it says what is left, no banner`, new RegExp(`^${re(left)} left`).test(spaced) && !(await page.$("[data-quota-banner]")), spaced.slice(0, 40))
          check(`${name}/${s}: home card says what is left`, new RegExp(`^Hosted allowance ${re(left)} left`).test(card.text), card.text.slice(0, 50))
          const links = await live.locator("a").count()
          check(`${name}/${s}: ${s === "nearly" ? "past 80% it offers add bandwidth" : "no upsell below 80%"}`, s === "nearly" ? links === 2 : links === 0, `${links} links`)
        }
      }
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)
      check(`${name}/${s}: no horizontal overflow`, overflow <= 0, `${overflow}px`)
      if (process.env.SHOTS) await page.screenshot({ path: `${process.env.SHOTS}/usage-${name}-${s}.png` })
      await context.close()
    }
  }
} finally {
  await browser?.close()
  next.kill("SIGTERM")
  api.close()
}
console.log(failed ? `${failed} failed` : "all passed")
process.exit(failed ? 1 : 0)
