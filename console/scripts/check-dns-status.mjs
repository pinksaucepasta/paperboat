// Browser check of the per-record DNS status on a custom domain's panel (Network page > Domains). It runs the console from this
// checkout against a small stub control plane (no real server, nothing is sent or changed) and walks four domain states:
//   healthy (every record verified), wrong (DNS has an address that is not a region machine), missing (one region machine is not
//   in DNS), checking (nothing looked up yet). The optional root-domain record is `not_checked` in all of them: it must never show.
// usage: node scripts/check-dns-status.mjs          PW_MODULE=<playwright-core index.js> PW_CHROMIUM=<chrome path> when not installed
// It starts `next dev` on a free port with PAPERBOAT_SERVER_URL pointing at the stub. Exits 1 when a step fails.
import { createServer } from "node:http"
import { spawn } from "node:child_process"
import { createServer as net } from "node:net"

const playwright = await import(process.env.PW_MODULE ?? "playwright-core")
const chromium = playwright.chromium ?? playwright.default.chromium
const freePort = () => new Promise((resolve) => { const s = net().listen(0, "127.0.0.1", () => { const { port } = s.address(); s.close(() => resolve(port)) }) })

const HOST = "shop.example.test"
const A = "203.0.113.10"
const B = "203.0.113.11"
const STRAY = "198.51.100.7"
const rec = (name, type, value, purpose, record_status, observed_values, required = purpose !== "apex_routing") =>
  ({ name, type, value, purpose, required, record_status, observed_values })
const verification = (status) => rec(`_acme-challenge.${HOST}`, "CNAME", "token.challenge.example.test", "verification", status, status === "pending" ? [] : ["token.challenge.example.test"])
const apex = [rec(HOST, "A", A, "apex_routing", "not_checked", []), rec(HOST, "A", B, "apex_routing", "not_checked", [])]
const scenarios = {
  healthy: { health: "healthy", records: [verification("verified"), rec(`*.${HOST}`, "A", A, "routing", "verified", [A, B]), rec(`*.${HOST}`, "A", B, "routing", "verified", [A, B]), ...apex] },
  wrong: { health: "wrong_address", records: [verification("verified"), rec(`*.${HOST}`, "A", A, "routing", "wrong_address", [A, STRAY]), rec(`*.${HOST}`, "A", B, "routing", "wrong_address", [A, STRAY]), ...apex] }, // the server marks every record of a name wrong_address
  missing: { health: "missing", records: [verification("verified"), rec(`*.${HOST}`, "A", A, "routing", "verified", [A]), rec(`*.${HOST}`, "A", B, "routing", "missing", [A]), ...apex] },
  checking: { health: "checking", records: [verification("pending"), rec(`*.${HOST}`, "A", A, "routing", "pending", []), rec(`*.${HOST}`, "A", B, "routing", "pending", []), ...apex] },
}
let state = "healthy"
const domain = () => ({
  id: "wsd_000000000001", hostname: HOST, workspace: "personal", state: "verified", observed_records: [], next_check_at: new Date(Date.now() + 60_000).toISOString(),
  last_checked_at: new Date().toISOString(), certificate: { state: "ready", expires_at: new Date(Date.now() + 60 * 86400_000).toISOString() }, used_by: [], created_at: new Date().toISOString(),
  records: scenarios[state].records, dns_health: scenarios[state].health,
})
const api = createServer((request, response) => {
  const path = new URL(request.url, "http://x").pathname.replace(/^\/v1\//, "")
  const answers = {
    me: { id: "usr_1", email: "ana@example.test", display_name: "Ana", status: "active", role: "user", workos_subject: "sub_1" },
    workspaces: { items: [{ id: "personal", name: "Personal", kind: "personal", role: "owner" }] },
    domains: { items: [domain()] },
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
const squash = (text) => text.replace(/\s+/g, " ")
let browser
try {
  const origin = `http://127.0.0.1:${port}`
  for (let i = 0; i < 120; i++) { if (await fetch(origin + "/auth/sign-in").then(() => true, () => false)) break; await new Promise((r) => setTimeout(r, 1000)) }
  browser = await chromium.launch(process.env.PW_CHROMIUM ? { executablePath: process.env.PW_CHROMIUM } : {})
  for (const [name, width] of [["desktop", 1440], ["phone", 390]]) {
    for (const s of Object.keys(scenarios)) {
      state = s
      const context = await browser.newContext({ viewport: { width, height: 900 } })
      await context.addCookies([{ name: "pb-dev-session", value: "stub", url: origin }])
      await context.grantPermissions(["clipboard-read", "clipboard-write"], { origin }).catch(() => {})
      const page = await context.newPage()
      const seen = []
      page.on("pageerror", (error) => seen.push(`pageerror ${error.message.slice(0, 200)}`))
      page.on("response", (response) => response.status() >= 400 && seen.push(`${response.status()} ${new URL(response.url()).pathname}`))
      await page.goto(`${origin}/network`, { timeout: 120_000 })
      const clicked = await page.getByRole("button", { name: HOST, exact: true }).click({ timeout: 60_000 }).then(() => "", (error) => error.message.split("\n")[0])
      const panel = await page.waitForSelector('[data-slot="sheet-content"]', { timeout: clicked ? 1_000 : 60_000 }).catch(() => null)
      const why = panel ? "" : `${clicked || "clicked, no panel"} at ${page.url()}: ${squash((await page.locator("body").innerText().catch(() => "")).slice(0, 300))} | ${[...new Set(seen)].join(", ")}`
      check(`${name}/${s}: the domain's panel opens`, Boolean(panel), why)
      if (!panel) { if (process.env.SHOTS) await page.screenshot({ path: `${process.env.SHOTS}/dns-${name}-${s}-fail.png` }); await context.close(); continue }
      const text = squash(await panel.innerText())
      const health = await panel.$eval("[data-dns-health]", (e) => e.getAttribute("data-dns-health"))
      const healthText = squash(await panel.$eval("[data-dns-health]", (e) => e.innerText))
      const pills = await panel.$$eval("[data-dns-record] [data-dns-status]", (e) => e.map((p) => p.getAttribute("data-dns-status")))
      const notes = await panel.$$eval("[data-dns-note]", (e) => e.map((n) => n.innerText.replace(/\s+/g, " ")))
      const expectHealth = { healthy: ["healthy", "Healthy"], wrong: ["wrong_address", "Wrong address"], missing: ["missing", "Missing"], checking: ["checking", "Checking"] }[s]
      check(`${name}/${s}: the header pill says ${expectHealth[1]}`, health === expectHealth[0] && healthText === expectHealth[1], `${health} ${healthText}`)
      check(`${name}/${s}: the root-domain record is neither shown nor counted`, (await panel.$$("[data-dns-record=apex_routing]")).length === 0 && !/Serves the domain itself/.test(text))
      const expectPills = { healthy: "verified,verified,verified", wrong: "verified,wrong_address,wrong_address", missing: "verified,verified,missing", checking: "pending,pending,pending" }[s]
      check(`${name}/${s}: each record has its own pill`, pills.join() === expectPills, pills.join())
      const labels = { healthy: "Verified", wrong: "Wrong address", missing: "Missing", checking: "Checking" }[s]
      check(`${name}/${s}: the pill says ${labels}`, new RegExp(labels).test(text))
      if (s === "wrong") {
        check(`${name}/${s}: it names the stray IP and says to remove it`, notes.some((n) => n.includes(STRAY) && /not one of your region machines/.test(n) && /remove it/.test(n) && !n.includes(A)), notes.join(" | "))
      } else if (s === "missing") {
        check(`${name}/${s}: it says the missing machine only costs the backup`, notes.length === 1 && /still work/.test(notes[0]) && /backup/.test(notes[0]), notes.join(" | "))
      } else {
        check(`${name}/${s}: no problem notes`, notes.length === 0, notes.join(" | "))
      }
      const copies = await panel.$$eval("button[aria-label^='Copy']", (b) => b.length)
      check(`${name}/${s}: every name and value has a copy button`, copies === 6, `${copies}`)
      if (s === "healthy" && name === "desktop") {
        await (await panel.$$("button[aria-label='Copy value']"))[1].click()
        const copied = await page.evaluate(() => navigator.clipboard.readText()).catch(() => "")
        check(`${name}/${s}: the copy button copies the value`, copied === A, copied)
      }
      const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)
      check(`${name}/${s}: no horizontal overflow`, overflow <= 0, `${overflow}px`)
      if (process.env.SHOTS) await page.screenshot({ path: `${process.env.SHOTS}/dns-${name}-${s}.png` })
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
