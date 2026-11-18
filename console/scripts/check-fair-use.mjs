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
const billing = () => {
  const used = { under: 40 * GB, nearly: 85 * GB, throttled: 104 * GB, own: 3 * GB }[state]
  const ends = new Date(Date.now() + 9 * 86400_000).toISOString()
  return {
    id: "personal:usr_1", state: "active", seats_bought: 1, members: 1, can_manage: true, cancel_at_period_end: false, seats_used: 1,
    usage_bytes: used, allowance_bytes: 100 * GB, allowance_period_start: new Date(Date.now() - 21 * 86400_000).toISOString(), allowance_period_end: ends,
    relay_mbit: 10, relay_included_bytes: 100 * GB, relay_slow_mbit: 1,
    topup_bytes_remaining: 0, topup_bytes_purchased: 0, topups: [], granted: false, throttled: state === "throttled", throttle_lifted: false, throttle_mbit: 1,
    limits: state === "own"
      ? { machines: { used: 1, limit: 100 }, publications: { used: 0, limit: null, self_hosted: true }, browser_terminals: { used: 0, limit: null, self_hosted: true }, public_tcp: { used: 0, limit: null, self_hosted: true } }
      : { machines: { used: 1, limit: 100 }, publications: { used: 0, limit: 100 }, browser_terminals: { used: 0, limit: 10 }, public_tcp: { used: 0, limit: 1 } },
  }
}
const usage = () => ({
  start: new Date(Date.now() - 86400_000).toISOString(), end: new Date().toISOString(), updated_at: new Date().toISOString(),
  coverage: { edge: true, native: true, note: "" }, totals: { upload_bytes: 0, download_bytes: 0, by_path: [] }, resources: [], series: [],
  pagination: { limit: 200, next_cursor: "" },
})
const api = createServer((request, response) => {
  const path = new URL(request.url, "http://x").pathname.replace(/^\/v1\//, "")
  const answers = {
    me: { id: "usr_1", email: "ana@example.test", display_name: "Ana", status: "active", role: "user", workos_subject: "sub_1" },
    workspaces: { items: [{ id: "personal", name: "Personal", kind: "personal", role: "owner" }] },
    "billing/workspaces/personal": billing(),
    usage: usage(),
    announcements: { banner: null, notices: [], unread: 0 },
  }
  const data = answers[path]
  response.writeHead(data ? 200 : 404, { "content-type": "application/json" })
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
    for (const s of ["under", "nearly", "throttled", "own"]) {
      state = s
      const context = await browser.newContext({ viewport: { width, height: 900 } })
      await context.addCookies([{ name: "pb-dev-session", value: "stub", url: origin }])
      const page = await context.newPage()
      await page.goto(`${origin}/usage`, { timeout: 120_000 })
      const panel = await page.waitForSelector("[data-fair-use]", { timeout: 60_000 }).catch(() => null)
      check(`${name}/${s}: the usage page shows the public-traffic panel`, Boolean(panel))
      const text = ((await panel?.innerText()) ?? "").replace(/\s+/g, " ")
      const text0 = text
      check(`${name}/${s}: no asterisks`, !/\*/.test(text0))
      if (s === "own") {
        check(`${name}/${s}: it says Unlimited (your own edges) and that nothing is counted`, /Unlimited \(your own edges\)/.test(text) && /never counted or limited by Paperboat/.test(text), text.slice(0, 90))
        check(`${name}/${s}: no upsell, no banner`, (await panel.$$eval("a", (a) => a.length)) === 0 && !(await page.$("[data-quota-banner]")))
      } else {
        const used = { under: "40", nearly: "85", throttled: "104" }[s]
        check(`${name}/${s}: it shows what is used of the included amount`, new RegExp(`${used}\\.?\\d* GB of 100 GB included`).test(text), text.slice(0, 90))
        check(`${name}/${s}: it says plainly what happens after: reduced speed, billing date, never cut off or billed extra`, /100 GB a month of public traffic included/.test(text) && /speed is reduced to 1 Mbit\/s until your next billing date\. Never cut off, never billed extra\./.test(text) && /Relays never count against this and are never cut off: 10 Mbit\/s for the first 100 GB a month, then 1 Mbit\/s\./.test(text) && /Run your own edge or relay \(free, open source\): no limits\./.test(text))
        if (s === "throttled") {
          check(`${name}/${s}: it says reduced to 1 Mbit/s until the reset date`, /Reduced to 1 Mbit\/s until/.test(text), text.slice(0, 90))
          const links = (await panel.$$eval("a", (a) => a.map((e) => `${e.textContent.trim()}:${e.getAttribute("href")}`))).join(" ")
          check(`${name}/${s}: it offers add bandwidth and change plan`, /Add bandwidth:\/billing/.test(links) && /Change plan:\/billing/.test(links), links)
          const banner = await page.waitForSelector("[data-quota-banner]", { timeout: 30_000 }).catch(() => null)
          const bannerText = ((await banner?.innerText()) ?? "").replace(/\s+/g, " ")
          check(`${name}/${s}: the banner is calm: reduced, never cut off`, /reduced to 1 Mbit\/s/.test(bannerText) && /Never cut off, never billed extra/.test(bannerText) && !/paused|stopped|exhausted|\*/i.test(bannerText), bannerText.slice(0, 80))
        } else {
          check(`${name}/${s}: full speed, no banner`, /Full speed/.test(text) && !(await page.$("[data-quota-banner]")))
          const links = await panel.$$eval("a", (a) => a.length)
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
