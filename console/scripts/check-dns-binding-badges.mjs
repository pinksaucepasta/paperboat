// Browser check that a tunnel's and a preview's own domain show the same DNS badge as its entry in the Domains list: the entry
// with the same name or, for a name under a listed domain, that domain. Without an entry it says to add the domain on the Network page.
// It runs the console from this checkout against a stub control plane (no real server, nothing is sent or changed) with four states:
// healthy, wrong address (a stray IP in DNS), missing (a region machine absent from DNS) and not added (no list entry).
// usage: node scripts/check-dns-binding-badges.mjs     PW_MODULE=<playwright-core index.js> PW_CHROMIUM=<chrome path> when not installed
// Exits 1 when a step fails.
// LIVE MODE (read-only, set ORIGIN, or LIVE=1): no stub and no `next dev`; ORIGIN=<real console> DOMAIN=<real test domain, with at least
// one tunnel or preview bound to a name under it> STATE=<storage state of the signed-in test account> (or EMAIL + PASSWORD_FILE, the stored
// WorkOS password) [EXPECT_HEALTH=healthy] node scripts/check-dns-binding-badges.mjs. It reads the domain's badge in the Domains list, then
// the badge on every tunnel page (and the Previews page) that the domain's "Used by" list names, and requires them to agree.
import { createServer } from "node:http"
import { readFileSync } from "node:fs"
import { spawn } from "node:child_process"
import { createServer as net } from "node:net"
import * as live from "./dns-live.mjs"

const playwright = await import(process.env.PW_MODULE ?? "playwright-core")
const chromium = playwright.chromium ?? playwright.default.chromium
const freePort = () => new Promise((resolve) => { const s = net().listen(0, "127.0.0.1", () => { const { port } = s.address(); s.close(() => resolve(port)) }) })

if (live.liveMode()) {
  const cfg = live.liveConfig()
  let bad = 0
  const verdict = (step, ok, detail = "") => { if (!ok) bad++; console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`) }
  const lbrowser = await live.launch(playwright)
  try {
    const context = await live.signedInContext(lbrowser, cfg, { width: 1280, height: 900 })
    const { panel, why } = await live.openDomainPanel(context, cfg)
    verdict(`live/domains list: the entry of ${cfg.domain} opens`, Boolean(panel), why)
    if (panel) {
      const r = await live.readPanel(panel)
      verdict(`live/domains list: the entry shows a known DNS health`, live.HEALTHS.includes(r.health), String(r.health))
      if (cfg.expect) verdict(`live/domains list: the entry says ${cfg.expect}`, r.health === cfg.expect, String(r.health))
      verdict(`live/domains list: a tunnel or a preview is bound under the domain`, r.tunnelLinks.length > 0 || r.usesPreview, "the test domain's Used by list is empty: bind a tunnel or preview name under it")
      const pages = [...r.tunnelLinks.map((href) => ["tunnel", href]), ...(r.usesPreview ? [["preview", "/previews"]] : [])]
      for (const [kind, path] of pages) {
        const page = await context.newPage()
        await page.goto(`${cfg.origin}${path}`, { timeout: 120_000 })
        const first = await page.waitForSelector("[data-dns-health]", { timeout: 60_000 }).catch(() => null)
        verdict(`live/${kind} ${path}: the domain row shows a DNS badge`, Boolean(first), first ? "" : live.squash((await page.locator("main").innerText().catch(() => "")).slice(0, 300)))
        if (first) {
          const badges = await page.$$eval("[data-dns-health]", (e) => e.map((b) => [b.getAttribute("data-dns-health"), b.innerText.replace(/\s+/g, " ")]))
          verdict(`live/${kind} ${path}: every badge is a known DNS health with its label`, badges.every(([h, t]) => live.HEALTHS.includes(h) && t === live.LABELS[h]), badges.map((b) => b.join(":")).join(","))
          verdict(`live/${kind} ${path}: a badge says ${r.health}, like the Domains list`, badges.some(([h]) => h === r.health), badges.map((b) => b[0]).join(","))
          const href = await page.locator("a:has([data-dns-health], [data-dns-not-added])").first().getAttribute("href")
          verdict(`live/${kind} ${path}: the badge opens the Domains list`, href === "/network#domains", String(href))
        }
        if (process.env.SHOTS) await page.screenshot({ path: `${process.env.SHOTS}/binding-live-${kind}.png` })
        await page.close()
      }
    }
    await context.close()
  } finally { await lbrowser.close() }
  console.log(bad ? `${bad} failed` : "all passed")
  process.exit(bad ? 1 : 0)
}

// A valid object for a schema of the API description: required fields only, the first enum value, empty lists.
const doc = JSON.parse(readFileSync(new URL("../../server/docs/openapi.json", import.meta.url), "utf8"))
const synth = (schema) => {
  if (schema.$ref) return synth(doc.components.schemas[schema.$ref.split("/").pop()])
  if ("const" in schema) return schema.const
  if (schema.enum) return schema.enum[0]
  if (schema.oneOf || schema.anyOf) return synth((schema.oneOf ?? schema.anyOf)[0])
  const own = Object.fromEntries((schema.required ?? []).map((key) => [key, synth(schema.properties?.[key] ?? {})]))
  if (schema.allOf) return Object.assign({}, ...schema.allOf.map(synth), own)
  if (schema.type === "object" || schema.properties) return own
  if (schema.type === "array") return []
  if (schema.type === "integer" || schema.type === "number") return schema.minimum ?? 1
  if (schema.type === "boolean") return false
  return schema.format === "date-time" ? new Date().toISOString() : "x"
}
const make = (name, over) => ({ ...synth({ $ref: `#/components/schemas/${name}` }), ...over })

const HOST = "shop.example.test"
const A = "203.0.113.10"
const B = "203.0.113.11"
const STRAY = "198.51.100.7"
const rec = (name, type, value, purpose, record_status, observed_values) =>
  ({ name, type, value, purpose, required: purpose !== "apex_routing", record_status, observed_values })
const scenarios = {
  healthy: { health: "healthy", records: [rec(`*.${HOST}`, "A", A, "routing", "verified", [A, B]), rec(`*.${HOST}`, "A", B, "routing", "verified", [A, B])] },
  wrong: { health: "wrong_address", records: [rec(`*.${HOST}`, "A", A, "routing", "wrong_address", [A, STRAY]), rec(`*.${HOST}`, "A", B, "routing", "wrong_address", [A, STRAY])] },
  missing: { health: "missing", records: [rec(`*.${HOST}`, "A", A, "routing", "verified", [A]), rec(`*.${HOST}`, "A", B, "routing", "missing", [A])] },
  notadded: { health: "healthy", records: [] },
}
let state = "healthy"
const iso = new Date().toISOString()
const workspaceDomain = () => make("WorkspaceDomain", {
  id: "wsd_000000000001", hostname: state === "notadded" ? "other.example.test" : HOST, workspace: "personal", state: "verified", observed_records: [], next_check_at: iso, last_checked_at: iso,
  certificate: { state: "ready" }, used_by: [], created_at: iso, records: scenarios[state].records, dns_health: scenarios[state].health,
})
const binding = { state: "ready", dns: { target: "t.example.test" }, certificate: { state: "ready" } }
const answers = (path) => ({
  me: { id: "usr_1", email: "ana@example.test", display_name: "Ana", status: "active", role: "user", workos_subject: "sub_1" },
  workspaces: { items: [{ id: "personal", name: "Personal", kind: "personal", role: "owner" }] },
  domains: { items: [workspaceDomain()] },
  announcements: { banner: null, notices: [], unread: 0 },
  "tunnels/tn1": make("Tunnel", { id: "tn1", name: "api tunnel", desired_state: "active" }),
  "tunnels/tn1/routes": { items: [make("TunnelRoute", { id: "rt1", tunnel_id: "tn1", desired_state: "active", priority: 1, protocol: "http", host_match: { type: "exact", hostname: HOST }, origin: { scheme: "http", address: "localhost:3000", preserve_host: false } })] },
  "tunnels/tn1/domains": { items: [make("TunnelDomain", { ...binding, hostname: HOST, id: "dom1", tunnel_id: "tn1", route_id: "rt1" })] },
  "previews/pv1": make("PreviewLease", { id: "pv1", endpoint: `https://${HOST}`, state: "ready", domains: [make("PreviewDomain", { ...binding, hostname: `app.${HOST}`, id: "pdom1", preview_id: "pv1" })] }),
})[path]
const api = createServer((request, response) => {
  const path = new URL(request.url, "http://x").pathname.replace(/^\/v1\//, "")
  const data = answers(path)
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
const expected = { healthy: ["healthy", "Healthy"], wrong: ["wrong_address", "Wrong address"], missing: ["missing", "Missing"] }
const notAdded = "Not added — add it on the Network page"
let browser
try {
  const origin = `http://127.0.0.1:${port}`
  for (let i = 0; i < 120; i++) { if (await fetch(origin + "/auth/sign-in").then(() => true, () => false)) break; await new Promise((r) => setTimeout(r, 1000)) }
  browser = await chromium.launch(process.env.PW_CHROMIUM ? { executablePath: process.env.PW_CHROMIUM } : {})
  for (const s of Object.keys(scenarios)) {
    state = s
    for (const [kind, path] of [["tunnel", "/tunnels/tn1"], ["preview", "/previews/pv1"]]) {
      const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
      await context.addCookies([{ name: "pb-dev-session", value: "stub", url: origin }])
      const page = await context.newPage()
      await page.goto(`${origin}${path}`, { timeout: 120_000 })
      const badge = await page.waitForSelector(s === "notadded" ? "[data-dns-not-added]" : "[data-dns-health]", { timeout: 60_000 }).catch(() => null)
      check(`${s}/${kind}: the domain row shows a DNS badge`, Boolean(badge), badge ? "" : squash((await page.locator("main").innerText().catch(() => "")).slice(0, 300)))
      if (badge) {
        if (s === "notadded") {
          check(`${s}/${kind}: it says to add the domain on the Network page`, squash(await badge.innerText()) === notAdded)
        } else {
          const [health, label] = [await badge.getAttribute("data-dns-health"), squash(await badge.innerText())]
          check(`${s}/${kind}: it says ${expected[s][1]}, like the Domains list${kind === "preview" ? " (the name sits under the listed domain)" : ""}`, health === expected[s][0] && label === expected[s][1], `${health} ${label}`)
        }
        const href = await page.locator("a:has([data-dns-health], [data-dns-not-added])").first().getAttribute("href")
        check(`${s}/${kind}: the badge opens the Domains list`, href === "/network#domains", String(href))
      }
      if (process.env.SHOTS) await page.screenshot({ path: `${process.env.SHOTS}/binding-${s}-${kind}.png` })
      await context.close()
    }
    if (s === "notadded") continue
    const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
    await context.addCookies([{ name: "pb-dev-session", value: "stub", url: origin }])
    const page = await context.newPage()
    await page.goto(`${origin}/network`, { timeout: 120_000 })
    await page.getByRole("button", { name: HOST, exact: true }).click({ timeout: 60_000 })
    const panel = await page.waitForSelector('[data-slot="sheet-content"]', { timeout: 60_000 })
    const header = await panel.$eval("[data-dns-health]", (e) => e.getAttribute("data-dns-health"))
    check(`${s}/domains list: the entry says ${expected[s][1]}`, header === expected[s][0], header)
    await context.close()
  }
} finally {
  await browser?.close()
  next.kill("SIGTERM")
  api.close()
}
console.log(failed ? `${failed} failed` : "all passed")
process.exit(failed ? 1 : 0)
