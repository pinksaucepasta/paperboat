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
//   ended, first paint  the page never shows the plan it must take back: no frame reads "Pro" before "Free" (a recorder watches every change)
//   checkout return     ?checkout=done on a granted workspace says "Confirming payment…", offers no plan to buy meanwhile, drops the
//                       parameter from the address, and shows the paid plan once the payment is confirmed
//   refused sign-in     /auth/failed?reason=account_banned|ip_blocked names the reason and the support reference; anything else stays plain
//   switch period     "Switch to yearly" shows the plan's own yearly price from the catalog, never an add-on's (the catalog lists add-ons first)
//   activity filters  a filter chosen right after a client navigation (while the server renders) survives the page the server then sends
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
const personalProducts = [product("pro-month", "personal_subscription", "month", 900), product("sso-year", "team_addon", "year", 100000), product("pro-year", "personal_subscription", "year", 9000), { ...product("topup", "top_up", "one_time", 500), unit_bytes: 1e11, max_quantity: 100 }]
const teamProducts = [product("team-month", "team_subscription", "month", 1500), product("scim-year", "team_addon", "year", 100000), product("team-year", "team_subscription", "year", 15000), { ...product("topup", "top_up", "one_time", 500), unit_bytes: 1e11, max_quantity: 100 }]
const limits = { machines: { used: 2, limit: 100 }, publications: { used: 1, limit: 10 }, browser_terminals: { used: 0, limit: 10 }, public_tcp: { used: 0, limit: 10 } }
const base = { usage_bytes: 5e9, allowance_bytes: 1e12, allowance_period_start: iso(-5 * day), allowance_period_end: iso(25 * day), topup_bytes_remaining: 0, topup_bytes_purchased: 0, topups: [], limits, can_manage: true, cancel_at_period_end: false, granted: false, throttled: false, throttle_lifted: false, throttle_mbit: 10, relay_mbit: 10, sso_addon: false, scim_addon: false }
const states = {
  granted: (kind) => ({ ...base, id: kind === "team" ? "team:tg" : "personal:u", state: "active", product_code: kind === "team" ? "team-month" : "pro-month", product: (kind === "team" ? teamProducts : personalProducts)[0], seats_bought: kind === "team" ? 10 : 0, seats_used: kind === "team" ? 12 : 0, members: kind === "team" ? 12 : 1, period_start: iso(-day), period_end: iso(9 * day), cancel_at_period_end: true, granted: true, granted_until: iso(9 * day) }),
  ended: (kind) => ({ ...states.granted(kind), period_start: iso(-11 * day), period_end: iso(-day), granted: false, granted_until: undefined }),
  exhausted: (kind) => ({ ...states.granted(kind), usage_bytes: 1e12, throttled: true }),
  failed: (kind) => ({ ...states.paid(kind), state: "past_due", period_start: iso(-31 * day), period_end: iso(-day), paid_benefits_until: iso(5 * day) }),
  member: (kind) => ({ ...states.granted(kind), can_manage: false }),
  exhaustedPaid: (kind) => ({ ...states.paid(kind), usage_bytes: 1e12, throttled: true }),
  extras: (kind) => ({ ...states.paid(kind), allowance_granted_bytes: 2.5e11, allowance_bytes: 1.25e12, limits: { ...limits, machines: { used: 2, limit: 500, granted: true } }, ...(kind === "team" ? { seats_granted: 20 } : {}) }),
  paid: (kind) => ({ ...states.granted(kind), cancel_at_period_end: false, granted: false, granted_until: undefined, period_end: iso(20 * day), paid_benefits_until: iso(20 * day) }),
}
const world = { mode: "granted", hold: null } // hold: a promise the activity answer waits for
const seen = []
function route(url) {
  const p = url.pathname
  if (p === "/v1/me") return { id: "u", email: "owner@example.test", display_name: "Owner", status: "active", role: "user", workos_subject: "wo_u" }
  if (p === "/v1/workspaces") return { items: [{ id: "personal", name: "Personal", kind: "personal", role: "owner" }, { id: "tg", name: "Team G", kind: "team", role: "owner" }] }
  if (p === "/v1/client-configuration") return { purchase_consent: { version: "2026-11-19", text: "Start the service now." } }
  if (p === "/v1/billing/workspaces/personal") return states[world.mode]("personal")
  if (p === "/v1/billing/workspaces/teams/tg") return states[world.mode]("team")
  if (p === "/v1/billing/workspaces/personal/products") return { items: personalProducts, pagination: { limit: 50, offset: 0, total: personalProducts.length, next_offset: null } }
  if (p === "/v1/billing/workspaces/teams/tg/products") return { items: teamProducts, pagination: { limit: 50, offset: 0, total: teamProducts.length, next_offset: null } }
  // Everything else the shell asks for (invitations, the team inbox, ...): an empty list.
  return { items: [], pagination: { limit: 10, offset: 0, total: 0, next_offset: null } }
}
const fixture = http.createServer(async (req, res) => {
  const url = new URL(req.url, `http://127.0.0.1:${FIXTURE_PORT}`)
  seen.push(`${req.method} ${url.pathname}`)
  if (url.pathname === "/v1/activity") await world.hold
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
let currentStep = ""
const step = async (name, fn) => {
  currentStep = name
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
  // Records every state of the Plan row, so a wrong plan shown for a moment (before hydration) cannot pass unseen.
  await context.addInitScript(() => {
    window.__planRows = []
    const record = () => {
      for (const row of document.querySelectorAll("main [data-slot], main div")) {
        const text = row.textContent ?? ""
        if (/^Plan(Pro|Team|Free)/.test(text.replace(/\s+/g, "")) && text.length < 80) window.__planRows.push(text.replace(/\s+/g, " ").trim())
      }
    }
    new MutationObserver(record).observe(document, { childList: true, subtree: true, characterData: true })
  })
  const page = await context.newPage()
  const errors = []
  page.on("pageerror", (error) => errors.push(`${String(error).slice(0, 60)} (in "${currentStep.slice(0, 50)}")`))
  const open = async (workspace, mode, ready) => {
    world.mode = mode
    await context.addCookies([{ name: "pb-workspace", value: workspace, url: ORIGIN }])
    await page.goto(`${ORIGIN}/billing`)
    // The skeleton has the same headings; the loaded page has the plan row filled in.
    await page.getByText("Traffic included").first().waitFor({ timeout: 30000 })
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
  await step("an ended grant never flashes the plan it must take back: the Plan row reads Free, and no earlier frame read Pro", async () => {
    await open("personal", "ended", /Plan ended/)
    const rows = await page.evaluate(() => window.__planRows)
    expect(rows.some((r) => /Plan\s*Free/.test(r)), `the recorder saw no Plan row: ${JSON.stringify(rows.slice(0, 5))}`)
    expect(!rows.some((r) => /Plan\s*(Pro|Team)/.test(r)), `a wrong plan was shown first: ${JSON.stringify(rows.slice(0, 5))}`)
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
    expect(/used its included traffic/.test(banner), `banner: ${banner.slice(0, 160)}`)
    expect(/Subscribe/.test(banner) && !/Add bandwidth/.test(banner) && !/Change plan/.test(banner), `banner links on a grant: ${banner}`)
    await shot("exhausted-granted")
  })
  await step("operator grants on a paid plan: extra bandwidth and an overridden limit read as granted, in the numbers", async () => {
    const text = await open("personal", "extras", /Traffic included/)
    expect(/5\.0 GB of 1\.3 TB used · 250 GB granted/.test(text) || /of 1\.25 TB used · 250 GB granted/.test(text), `allowance row: ${text.slice(0, 400)}`)
    expect(/Machines 2 of 500 · Granted/.test(text), `machines row: ${text.slice(0, 400)}`)
    expect((text.match(/ · Granted/g) ?? []).length === 1, "an unchanged limit reads as granted")
    const team = await open("tg", "extras", /Seats/)
    expect(/Seats granted 20 · 12 used/.test(team), `team seats: ${team.slice(0, 400)}`)
  })
  await step("returning from Checkout to a granted workspace says Confirming payment, offers nothing to buy meanwhile, clears the address, and shows the paid plan once confirmed", async () => {
    world.mode = "granted"
    await context.addCookies([{ name: "pb-workspace", value: "personal", url: ORIGIN }])
    await page.goto(`${ORIGIN}/billing?checkout=done`)
    await page.waitForFunction(() => /Confirming payment/.test(document.querySelector("main")?.innerText ?? ""), null, { timeout: 30000 })
    const waiting = (await page.locator("main").first().innerText()).replace(/\s+/g, " ")
    expect(!/Subscribe|Upgrade/.test(waiting) && !/Pro · Granted/.test(waiting), `while confirming: ${waiting.slice(0, 240)}`)
    expect(!page.url().includes("checkout="), `the address keeps the return parameter: ${page.url()}`)
    world.mode = "paid" // the webhook lands
    await page.waitForFunction(() => /Renews/.test(document.querySelector("main")?.innerText ?? "") && !/Confirming payment/.test(document.querySelector("main")?.innerText ?? ""), null, { timeout: 30000 })
    expect((await buttons()).includes("Manage billing"), "the paid plan has no Manage billing after the return")
  })
  await step("Switch to yearly shows the plan's own yearly price from the catalog, never an add-on's", async () => {
    for (const [workspace, products, plan] of [["personal", personalProducts, "pro-year"], ["tg", teamProducts, "team-year"]]) {
      await open(workspace, "paid", /Renews/)
      const catalog = products.find((p) => p.code === plan)
      const addons = products.filter((p) => p.catalog_type === "team_addon")
      expect(catalog && addons.length > 0 && products.indexOf(addons[0]) < products.indexOf(catalog), "the fixture catalog must list an add-on before the plan's yearly price")
      const button = (await buttons()).find((name) => /^Switch to yearly/.test(name))
      expect(button, `${workspace}: no Switch to yearly button: ${await buttons()}`)
      const price = Number(button.match(/\$([\d,]+(?:\.\d+)?)/)?.[1]?.replace(/,/g, ""))
      expect(price * 100 === catalog.unit_amount, `${workspace}: "${button}" is not the plan's yearly price ${catalog.unit_amount / 100}`)
      expect(!addons.some((a) => a.unit_amount === price * 100), `${workspace}: "${button}" is an add-on's price`)
    }
  })
  await step("an Activity filter chosen right after a client navigation survives the server-rendered page that replaces the static shell", async () => {
    world.mode = "paid"
    await context.addCookies([{ name: "pb-workspace", value: "personal", url: ORIGIN }])
    const prefetched = page.waitForResponse((r) => /\/activity\?_rsc=/.test(r.url()), { timeout: 30000 })
    await page.goto(`${ORIGIN}/billing`)
    await page.getByText("Traffic included").first().waitFor({ timeout: 30000 })
    await prefetched // the static shell of the Activity page is in the router cache
    let release
    world.hold = new Promise((resolve) => (release = resolve)) // the server's Activity render waits for this: no sleeping
    await page.getByRole("link", { name: "Activity", exact: true }).first().click()
    // The static shell shows the filters at once; the server render is held, so this choice is made on it.
    await page.getByRole("button", { name: "All results" }).click({ timeout: 30000 })
    await page.getByRole("menuitemradio", { name: "Only denied" }).click()
    await page.getByRole("button", { name: "Only denied" }).waitFor({ timeout: 5000 }).catch(() => expect(false, "the choice is not shown on the static shell"))
    expect(await page.locator(".shell-delay").count() > 0, "the server page had already replaced the shell: the choice was not made on the shell")
    release() // the server page now arrives and replaces the shell
    await page.waitForFunction(() => !document.querySelector(".shell-delay"), null, { timeout: 30000 })
    await page.getByRole("button", { name: "Only denied" }).waitFor({ timeout: 5000 }).catch(() => expect(false, "the filter was reset when the server page replaced the shell"))
    expect(page.url().includes("result=denied"), `the filter is not in the address: ${page.url()}`)
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
  await step("a sign-in the server refused on purpose says why, with its reference; any other failure keeps the plain message", async () => {
    const visitor = await browser.newContext() // signed out: a signed-in visitor is sent to the console instead
    const out = await visitor.newPage()
    const read = async (query) => {
      await out.goto(`${ORIGIN}/auth/failed${query}`)
      await out.waitForSelector("h1")
      return (await out.locator("body").innerText()).replace(/\s+/g, " ")
    }
    const banned = await read("?reason=account_banned&ref=support_abc-123")
    expect(/This account can’t sign in/.test(banned) && /support_abc-123/.test(banned) && (await out.locator('a[href="/auth/sign-in"]').count()) === 0, `banned page: ${banned.slice(0, 200)}`)
    const blocked = await read("?reason=ip_blocked&ref=support_x")
    expect(/Sign-in isn’t available from this network/.test(blocked) && /support_x/.test(blocked), `blocked page: ${blocked.slice(0, 200)}`)
    const plain = await read("?reason=nonsense&ref=<script>")
    expect(/We couldn’t sign you in/.test(plain) && (await out.locator('a[href="/auth/sign-in"]').count()) === 1 && !/script/.test(plain), `plain page: ${plain.slice(0, 200)}`)
    await visitor.close()
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
