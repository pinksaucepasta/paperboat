// Headless check of the customer console's Billing page for a workspace with a running support grant (F13 plan grants).
// It builds nothing: it runs the BUILT console (`NEXT_DIST_DIR=<dir> bun run build` first, then this) against a fixture control
// plane started here, so no WorkOS sign-in and no live account is needed. For each state of a workspace it checks what the
// page shows and that nothing dead is offered:
//   granted personal  "Granted" plan, "Granted until", no Ends/Renews, no Manage billing, no top-ups, Subscribe offered
//   granted team      seats read-only ("N granted · M used", "Subscribe to change seats."), no seat or top-up controls
//   ended grant       "Free", "Plan ended", plan offers back, nothing to manage
//   exhausted grant   the quota banner says Subscribe, not Add bandwidth (top-ups need a paid plan)
//   failed payment    a paid plan past its period with a failed payment is not shown as ended
//   member            a team member who cannot manage billing sees the grant but cannot start a subscription
//   paid (control)    Manage billing, Renews, top-ups and the quota banner's Add bandwidth ARE shown, so the checks above can fail
// usage: PW_CHROMIUM=<chromium> [PW_MODULE=<playwright-core index.mjs>] [NEXT_DIST_DIR=.next] [APP_PORT=3197] [FIXTURE_PORT=3196]
//        [SHOT_DIR=<dir>] node scripts/check-billing-grant.mjs
import { spawn } from "node:child_process"
import { mkdirSync } from "node:fs"
import http from "node:http"

const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const APP_PORT = Number(process.env.APP_PORT ?? 3197)
const FIXTURE_PORT = Number(process.env.FIXTURE_PORT ?? 3196)
const ORIGIN = `http://127.0.0.1:${APP_PORT}`
const shots = process.env.SHOT_DIR
if (shots) mkdirSync(shots, { recursive: true })
if (!process.env.PW_CHROMIUM) throw new Error("PW_CHROMIUM is not set; see the header of check-billing-grant.mjs")

// ---- fixture control plane -----------------------------------------------------------------------
const day = 86400e3
const iso = (ms) => new Date(Date.now() + ms).toISOString()
const product = (code, type, interval, amount) => ({ code, name: code.includes("team") ? "Team" : "Pro", catalog_type: type, unit_amount: amount, currency: "usd", interval, price_verified: true, version: 1 })
const personalProducts = [product("pro-month", "personal_subscription", "month", 900), product("pro-year", "personal_subscription", "year", 9000), { ...product("topup", "top_up", "one_time", 500), unit_bytes: 1e11, max_quantity: 100 }]
const teamProducts = [product("team-month", "team_subscription", "month", 1500), { ...product("topup", "top_up", "one_time", 500), unit_bytes: 1e11, max_quantity: 100 }]
const limits = { machines: { used: 2, limit: 100 }, publications: { used: 1, limit: 100 }, browser_terminals: { used: 0, limit: 10 }, public_tcp: { used: 0, limit: 10 } }
const base = { usage_bytes: 5e9, allowance_bytes: 1e12, allowance_period_start: iso(-5 * day), allowance_period_end: iso(25 * day), topup_bytes_remaining: 0, topup_bytes_purchased: 0, topups: [], limits, can_manage: true, cancel_at_period_end: false, granted: false }
const states = {
  granted: (kind) => ({ ...base, id: kind === "team" ? "team:tg" : "personal:u", state: "active", product_code: kind === "team" ? "team-month" : "pro-month", product: (kind === "team" ? teamProducts : personalProducts)[0], seats_bought: kind === "team" ? 10 : 0, seats_used: kind === "team" ? 12 : 0, members: kind === "team" ? 12 : 1, period_start: iso(-day), period_end: iso(9 * day), cancel_at_period_end: true, granted: true, granted_until: iso(9 * day) }),
  ended: (kind) => ({ ...states.granted(kind), period_start: iso(-11 * day), period_end: iso(-day), granted: false, granted_until: undefined }),
  exhausted: (kind) => ({ ...states.granted(kind), usage_bytes: 1e12 }),
  failed: (kind) => ({ ...states.paid(kind), state: "past_due", period_start: iso(-31 * day), period_end: iso(-day), paid_benefits_until: iso(5 * day) }),
  member: (kind) => ({ ...states.granted(kind), can_manage: false }),
  exhaustedPaid: (kind) => ({ ...states.paid(kind), usage_bytes: 1e12 }),
  paid: (kind) => ({ ...states.granted(kind), cancel_at_period_end: false, granted: false, granted_until: undefined, period_end: iso(20 * day), paid_benefits_until: iso(20 * day) }),
}
const world = { mode: "granted" }
const seen = []
function route(url) {
  const p = url.pathname
  if (p === "/v1/me") return { id: "u", email: "owner@example.test", display_name: "Owner", status: "active", role: "user", workos_subject: "wo_u" }
  if (p === "/v1/workspaces") return { items: [{ id: "personal", name: "Personal", kind: "personal", role: "owner" }, { id: "tg", name: "Team G", kind: "team", role: "owner" }] }
  if (p === "/v1/client-configuration") return {}
  if (p === "/v1/billing/workspaces/personal") return states[world.mode]("personal")
  if (p === "/v1/billing/workspaces/teams/tg") return states[world.mode]("team")
  if (p === "/v1/billing/workspaces/personal/products") return { items: personalProducts, pagination: { limit: 50, offset: 0, total: personalProducts.length, next_offset: null } }
  if (p === "/v1/billing/workspaces/teams/tg/products") return { items: teamProducts, pagination: { limit: 50, offset: 0, total: teamProducts.length, next_offset: null } }
  // Everything else the shell asks for (invitations, the team inbox, ...): an empty list.
  return { items: [], pagination: { limit: 10, offset: 0, total: 0, next_offset: null } }
}
const fixture = http.createServer((req, res) => {
  const url = new URL(req.url, `http://127.0.0.1:${FIXTURE_PORT}`)
  seen.push(`${req.method} ${url.pathname}`)
  const out = route(url)
  res.writeHead(200, { "content-type": "application/json" })
  res.end(JSON.stringify({ data: out }))
})
await new Promise((resolve) => fixture.listen(FIXTURE_PORT, "127.0.0.1", resolve))

// ---- the built console -----------------------------------------------------------------------------
const app = spawn("bun", ["x", "next", "start", "-p", String(APP_PORT), "-H", "127.0.0.1"], {
  env: { ...process.env, PAPERBOAT_SERVER_URL: `http://127.0.0.1:${FIXTURE_PORT}`, WORKOS_REDIRECT_URI: `${ORIGIN}/auth/callback`, NEXT_DIST_DIR: process.env.NEXT_DIST_DIR ?? ".next" },
  stdio: "ignore",
})
async function appUp() {
  for (let i = 0; i < 60; i++) {
    try {
      if ((await fetch(`${ORIGIN}/auth/sign-in`, { redirect: "manual" })).status < 500) return
    } catch {}
    await new Promise((r) => setTimeout(r, 500))
  }
  throw new Error("the console did not start; build it first (NEXT_DIST_DIR=<dir> bun run build)")
}

const results = []
const step = async (name, fn) => {
  try {
    await fn()
    results.push([true, name])
  } catch (error) {
    results.push([false, `${name}  (${String(error.message ?? error).split("\n")[0]})`])
  }
}
const expect = (cond, message) => {
  if (!cond) throw new Error(message)
}

let browser
try {
  await appUp()
  browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
  const context = await browser.newContext({ viewport: { width: 1280, height: 1000 } })
  await context.addCookies([{ name: "pb-dev-session", value: "fixture-session", url: ORIGIN }])
  const page = await context.newPage()
  const errors = []
  page.on("pageerror", (error) => errors.push(String(error)))
  const open = async (workspace, mode, ready) => {
    world.mode = mode
    await context.addCookies([{ name: "pb-workspace", value: workspace, url: ORIGIN }])
    await page.goto(`${ORIGIN}/billing`)
    // The skeleton has the same headings; the loaded page has the plan row filled in.
    await page.getByText("Hosted allowance").first().waitFor({ timeout: 30000 })
    await page.waitForFunction(() => !document.querySelector('[data-slot="skeleton"], .animate-pulse'), null, { timeout: 30000 })
    // Times (and so "ended") are only known once the page has hydrated; wait for the text that depends on them.
    await page.waitForFunction((pattern) => new RegExp(pattern).test(document.querySelector("main")?.innerText ?? ""), ready.source, { timeout: 30000 })
    return (await page.locator("main").first().innerText()).replace(/\s+/g, " ")
  }
  const buttons = async () => (await page.getByRole("button").allInnerTexts()).map((t) => t.trim())
  const shot = (name) => (shots ? page.screenshot({ path: `${shots}/${name}.png`, fullPage: true }) : undefined)

  await step("a granted personal plan: granted label and end date, no Ends/Renews, no price, no Manage billing, no top-ups, Subscribe offered", async () => {
    const text = await open("personal", "granted", /Granted until/)
    expect(/Pro · Granted/.test(text), `plan row: ${text.slice(0, 200)}`)
    expect(/Granted until/.test(text), "no Granted until row")
    expect(!/\b(Ends|Renews)\b/.test(text), "a granted plan shows Ends or Renews")
    expect(!/\$9/.test(text.split("Subscribe")[0] ?? ""), "the granted plan shows a price")
    const names = await buttons()
    expect(!names.includes("Manage billing"), "Manage billing is offered on a grant")
    expect(!names.some((n) => /switch|cancel|keep/i.test(n)), `plan actions on a grant: ${names}`)
    expect(!/Add bandwidth/.test(text), "top-ups are offered on a grant")
    expect(/Subscribe/.test(text) && names.includes("Subscribe"), "Subscribe is not offered")
    expect(!/Upgrade/.test(text), "a grant is offered Upgrade instead of Subscribe")
    await shot("granted-personal")
  })
  await step("a granted team plan: seats are read-only with the way to change them, no seat or top-up controls", async () => {
    const text = await open("tg", "granted", /Granted until/)
    expect(/10 granted · 12 used/.test(text), `seats row: ${text.slice(0, 300)}`)
    expect(/Subscribe to change seats\./.test(text), "no hint how to change seats")
    expect((await page.getByRole("button", { name: /Add a seat|Remove a seat|Buy seats|Remove seats/ }).count()) === 0, "seat controls on a grant")
    expect(!(await buttons()).includes("Manage billing") && !/Add bandwidth/.test(text), "Manage billing or top-ups on a team grant")
    await shot("granted-team")
  })
  await step("an ended grant reads as Free with Plan ended, and the plans are offered again", async () => {
    const text = await open("personal", "ended", /Plan ended/)
    expect(/Plan\s*Free/.test(text) && /Plan ended/.test(text), `ended plan: ${text.slice(0, 240)}`)
    expect(!/Granted/.test(text), "an ended grant still reads as granted")
    const names = await buttons()
    expect(!names.includes("Manage billing") && /Upgrade/.test(text), `ended plan controls: ${names}`)
    expect(!/Add bandwidth/.test(text) && !/\b(Ends|Renews)\b/.test(text), "an ended plan offers top-ups or shows Ends/Renews")
    expect(!names.some((n) => /switch|cancel|keep/i.test(n)), `plan actions on an ended plan: ${names}`)
    await shot("ended-personal")
  })
  await step("a past-due paid plan is not shown as ended: its failed payment and the way to fix it are", async () => {
    const text = await open("personal", "failed", /Payment failed/)
    expect(!/Plan ended/.test(text) && !/Plan\s*Free\b/.test(text), `a past-due plan reads as ended: ${text.slice(0, 200)}`)
    expect((await buttons()).includes("Update payment method"), "no way to fix the payment")
  })
  await step("a team member who cannot manage billing sees the grant but cannot start a subscription or buy anything", async () => {
    const text = await open("tg", "member", /Granted until/)
    expect(/Team · Granted/.test(text), "the member does not see the grant")
    const names = await buttons()
    expect(!names.includes("Manage billing"), "Manage billing for a member")
    expect(await page.getByRole("button", { name: "Subscribe", exact: true }).isDisabled(), "a member can start a subscription")
  })
  await step("an exhausted grant: the quota banner offers Subscribe, not Add bandwidth or Change plan", async () => {
    await open("personal", "exhausted", /Granted until/)
    const banner = (await page.locator("[data-quota-banner]").first().innerText({ timeout: 15000 })).replace(/\s+/g, " ")
    expect(/used its bandwidth|used its bandwidth for this period/.test(banner), `banner: ${banner.slice(0, 160)}`)
    expect(/Subscribe/.test(banner) && !/Add bandwidth/.test(banner) && !/Change plan/.test(banner), `banner links on a grant: ${banner}`)
    await shot("exhausted-granted")
  })
  await step("control: a paid plan shows Manage billing, Renews and top-ups, so the checks above can fail", async () => {
    const text = await open("personal", "paid", /Renews/)
    expect(!/Granted/.test(text) && /Renews/.test(text) && /Add bandwidth/.test(text), `paid plan: ${text.slice(0, 240)}`)
    expect((await buttons()).includes("Manage billing"), "a paid plan has no Manage billing")
    const team = await open("tg", "paid", /Renews/)
    expect(/Seats/.test(team) && (await page.getByRole("button", { name: "Add a seat" }).count()) === 1, "a paid team has no seat controls")
    await shot("paid-team")
    await open("personal", "exhaustedPaid", /Renews/)
    const banner = (await page.locator("[data-quota-banner]").first().innerText({ timeout: 15000 })).replace(/\s+/g, " ")
    expect(/Add bandwidth/.test(banner) && /Change plan/.test(banner), `a paid exhausted plan's banner: ${banner}`)
  })
  await step("no page errors, and nothing on the page is an error answer", async () => {
    expect(errors.length === 0, errors.join("; ").slice(0, 200))
    expect(!(await page.locator("main").first().innerText()).includes("Reference:"), "an error is shown on the billing page")
  })
} catch (error) {
  results.push([false, `check aborted  (${String(error.message ?? error).split("\n")[0]})`])
} finally {
  await browser?.close()
  app.kill()
  fixture.close()
}
let failed = 0
for (const [ok, name] of results) {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}`)
}
console.log(`${results.length - failed}/${results.length} passed`)
process.exit(failed || results.length === 0 ? 1 : 0)
