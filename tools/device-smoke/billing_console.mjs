// The live console's billing screens and its Stripe TEST-MODE round trips, for billing_checks.py.
// usage: ORIGIN=<console> STATE=<storage state of a THROWAWAY account> PW_CHROMIUM=<chromium> [PW_MODULE=...] node billing_console.mjs
//   card free|pro|throttled     F13-06: the plan page and the usage page's allowance card agree with the billing and usage APIs;
//                               console/scripts/check-usage-paths.js (Direct/Relay/Edge cards) runs on the same page
//   checkout                    F13-04C: Upgrade (Pro, monthly) on the console, Stripe test card, back to the paid overview
//   portal month|year           Manage billing: switch the billing interval in Stripe's portal, back to the console
//   portal cancel|keep          Cancel plan / Keep plan in Stripe's portal, back to the console
//   team-checkout TEAM SEATS    Create team (monthly) on the console, pay on test-mode Checkout, the team's paid overview
//   seats N                     the team's billing page: set N seats, read what it costs now, buy or remove
//   invoices                    the Stripe portal's invoice history; the step "invoices" carries the number of paid ones in "paid"
// With BILLING_TEAM=<team id> the plan, seats, portal and invoice commands act on that team's workspace instead of the personal one.
// Prints one JSON line {"steps":[{"step","ok","detail"}]}: every step is tried, none stops the rest. Exit 1 when one failed.
// Stripe pages are refused unless they are test-mode ones (stripe_pay.mjs; the portal: a test_ session AND the "Sandbox" badge).
import { readFileSync } from "node:fs"
import { fileURLToPath } from "node:url"
import { payTestCheckout } from "./stripe_pay.mjs"

export function formatBytes(bytes) {
  const units = ["B", "KB", "MB", "GB", "TB", "PB"]
  let value = bytes
  let unit = 0
  while (value >= 1000 && unit < units.length - 1) (value /= 1000), unit++
  return `${value.toFixed(unit === 0 || value >= 100 ? 0 : 1)} ${units[unit]}`
}

// "5.0 GB" -> 5e9; NaN when the text holds no amount.
export function parseBytes(text) {
  const found = /([\d.]+)\s*(B|KB|MB|GB|TB|PB)\b/.exec(text ?? "")
  return found ? Number(found[1]) * 1000 ** ["B", "KB", "MB", "GB", "TB", "PB"].indexOf(found[2]) : NaN
}

// A shown amount matches the API's: the page rounds to three digits and reads a moment apart from the check.
export const near = (shown, bytes) => Math.abs(parseBytes(shown) - bytes) <= Math.max(0.05 * bytes, 1000)

// "$12.50" -> 12.5, "-$3" -> -3
export const amount = (text) => Number(text.replace(/[$,]/g, ""))

export const speed = (mbit) => (mbit < 1 ? `${Math.round(mbit * 1000)} kbit/s` : `${mbit} Mbit/s`)

export const price = (product) => `$${product.unit_amount / 100}`

// The plan page's text against the billing overview: [step, ok, detail] rows.
export function planRows(text, billing) {
  const flat = text.replace(/\s+/g, " ")
  const plan = billing.product ? `${billing.product.name.replace(/^Paperboat\s+/, "").replace(/\s*\((month|year)\)$/, "")} · ${price(billing.product)}/${billing.product.interval}` : "Free"
  const included = /Traffic included ([\d.]+ \S?B) of ([\d.]+ \S?B) used/.exec(flat)
  return [
    ["plan screen names the plan", flat.includes(`Plan ${plan}`), `want ${plan}`],
    ["plan screen: traffic used and included match billing", Boolean(included) && near(included[1], billing.usage_bytes) && near(included[2], billing.allowance_bytes), included ? included[0] : "no 'Traffic included' row"],
    ["plan screen: machine limit matches billing", flat.includes(`Machines ${billing.limits.machines.used.toLocaleString("en-US")} of ${billing.limits.machines.limit.toLocaleString("en-US")}`), `limit ${billing.limits.machines.limit}`],
  ]
}

// The usage page's allowance card against the billing overview and the hosted usage of the same window.
export function cardRows(text, throttledAttr, billing, hosted) {
  const flat = text.replace(/\s+/g, " ")
  const amount = billing.allowance_bytes + billing.topup_bytes_purchased
  const pool = /([\d.]+ \S?B) of ([\d.]+ \S?B)(?! a month)/.exec(flat)
  const left = /([\d.]+ \S?B) left/.exec(flat)
  const legend = (label) => new RegExp(`${label} ([\\d.]+ \\S?B)`).exec(flat)?.[1]
  return [
    ["card: throttled state matches billing", throttledAttr === String(billing.throttled), `card ${throttledAttr}, billing ${billing.throttled}`],
    ["card: used of included matches billing", Boolean(pool) && near(pool[1], billing.usage_bytes) && near(pool[2], amount), pool ? pool[0] : "no 'X of Y'"],
    billing.throttled
      ? ["card: says it is slowed and to what speed", flat.includes(`Slowed to ${speed(billing.throttle_mbit)}`), `want 'Slowed to ${speed(billing.throttle_mbit)}'`]
      : ["card: shows what is left", Boolean(left) && near(left[1], Math.max(0, amount - billing.usage_bytes)), left ? left[0] : "no 'left' headline"],
    ["card: relay series matches hosted relay usage", near(legend("Relay"), hosted.relay), `card ${legend("Relay")}, API ${hosted.relay}`],
    ["card: edge series matches hosted edge usage", near(legend("Edge"), hosted.edge), `card ${legend("Edge")}, API ${hosted.edge}`],
    ["card: states the plan's included amount and its slowed speed", flat.includes(`${formatBytes(amount)} a month of traffic included`) && flat.includes(`reduced to ${speed(billing.throttle_mbit)}`), `want ${formatBytes(amount)} and ${speed(billing.throttle_mbit)}`],
    ["card: states the plan's relay speed", flat.includes(`up to ${speed(billing.relay_mbit)}`), `want ${speed(billing.relay_mbit)}`],
  ]
}

// the session's own secret parameter must be a test one; "test_" anywhere else in the address counts for nothing
export const testPortalURL = (url) => /^https:\/\/billing\.stripe\.com\/p\/session(\/[^?#]*)?\?([^#]*&)?secret=test_/.test(url ?? "")

async function main(argv) {
  const [command, argument, extra] = argv
  const team = process.env.BILLING_TEAM ?? ""
  const origin = process.env.ORIGIN
  const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
  const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
  const context = await browser.newContext({ storageState: process.env.STATE, viewport: { width: 1280, height: 1600 } })
  // the console's selected workspace is a cookie of its own
  if (team) await context.addCookies([{ name: "pb-workspace", value: team, url: origin }])
  const page = await context.newPage()
  const steps = []
  const add = (step, ok, detail = "") => steps.push({ step, ok: Boolean(ok), detail: String(detail).slice(0, 220) })
  const get = async (path) => {
    for (let attempt = 0; ; attempt++) {
      const reply = await page.request.get(`${origin}/api/v1/${path}`).catch((error) => ({ ok: () => false, status: () => String(error.message).slice(0, 60) }))
      if (reply.ok()) return (await reply.json()).data
      if (attempt === 5) throw new Error(`GET /v1/${path} answered ${reply.status()}`)
      await page.waitForTimeout(5000) // the live console's dev server drops a connection now and then
    }
  }
  const overview = (id = team) => get(id ? `billing/workspaces/teams/${id}` : "billing/workspaces/personal")
  const main_ = () => page.locator("main").last()
  const open = async (path, ready) => {
    for (let attempt = 0; ; attempt++) {
      try {
        await page.goto(origin + path, { waitUntil: "domcontentloaded", timeout: 180000 })
        await ready()
        return
      } catch (error) {
        if (attempt === 2) throw error
        await page.waitForTimeout(8000)
      }
    }
  }
  // Waits until the page's text satisfies `done` (or 90 s) and returns the last text either way.
  const settled = async (locator, done) => {
    let text = ""
    for (let i = 0; i < 45; i++) {
      text = await locator.innerText().catch(() => "")
      if (done(text)) break
      await page.waitForTimeout(2000)
    }
    return text
  }
  const openPlan = () => open("/billing", () => main_().getByText("Traffic included", { exact: true }).waitFor({ timeout: 90000 }))

  async function card(expected) {
    const billing = await overview()
    add(`billing is ${expected}`, expected === "pro" ? billing.product_code?.startsWith("pro-") && !billing.throttled : billing.state === "free" && billing.throttled === (expected === "throttled"),
      `state=${billing.state} product=${billing.product_code} throttled=${billing.throttled}`)
    await openPlan()
    const planText = await settled(main_(), (t) => planRows(t, billing).every(([, ok]) => ok))
    for (const [step, ok, detail] of planRows(planText, billing)) add(step, ok, detail)
    // while the page renders, a not yet shown copy of the card can exist for a moment: wait for the visible one, and count once settled
    const section = page.locator("[data-fair-use]:visible").first()
    await open("/usage", () => section.waitFor({ timeout: 90000 }))
    const query = new URLSearchParams({ start: billing.allowance_period_start, end: new Date().toISOString(), limit: "200", workspace: "personal" })
    const hosted = { direct: 0, relay: 0, edge: 0 }
    for (const p of (await get(`usage?${query}`)).totals.by_path) if (p.hosting === "managed") hosted[p.mode] += p.upload_bytes + p.download_bytes
    let rows = []
    for (let i = 0; i < 45; i++) {
      rows = cardRows(await section.innerText().catch(() => ""), await section.getAttribute("data-throttled").catch(() => null), billing, hosted)
      if (rows.every(([, ok]) => ok)) break
      await page.waitForTimeout(2000)
    }
    for (const [step, ok, detail] of rows) add(step, ok, detail)
    const shown = await page.locator("[data-fair-use]:visible").count()
    add("the usage page shows one allowance card", shown === 1, `${shown} visible`)
    await page.getByRole("button", { name: "7 days" }).or(page.getByRole("radio", { name: "7 days" })).or(page.getByRole("tab", { name: "7 days" })).first().click({ timeout: 15000 }).catch(() => {})
    await page.waitForTimeout(6000)
    const script = readFileSync(fileURLToPath(new URL("../../console/scripts/check-usage-paths.js", import.meta.url)), "utf8")
    const found = await page.evaluate(script)
    for (const s of found ?? []) add(`check-usage-paths: ${s.step}`, s.ok, s.detail)
    if (!found?.length) add("check-usage-paths ran", false, "no steps")
  }

  async function checkout() {
    await openPlan()
    const consent = main_().getByRole("checkbox").first()
    await consent.waitFor({ timeout: 60000 })
    for (let i = 0; i < 30 && !(await consent.isEnabled()); i++) await page.waitForTimeout(1000)
    const offer = main_().getByRole("button", { name: "Upgrade", exact: true })
    add("Upgrade is disabled until the purchase terms are ticked", await offer.isDisabled())
    await consent.click()
    add("the offered plan is Pro at $8/month", /Pro\s+\$8\/month/.test(await main_().innerText()))
    await offer.click()
    await page.waitForURL((u) => u.hostname === "checkout.stripe.com", { timeout: 90000, waitUntil: "commit" })
    add("Upgrade opens a Stripe test-mode Checkout", /\/pay\/cs_test_/.test(page.url()))
    await payTestCheckout(page) // refuses anything but a sandbox session
    await page.waitForURL((u) => u.href.startsWith(origin + "/billing"), { timeout: 180000, waitUntil: "commit" })
    add("Checkout returns to the console's billing page", true)
    // The plan activates when Stripe's test webhook reaches the server.
    const paid = await settled(main_(), (t) => /Plan\s+Pro · \$8\/month/.test(t) && !/Confirming payment/.test(t))
    add("the paid overview shows Pro · $8/month", /Plan\s+Pro · \$8\/month/.test(paid), paid.replace(/\s+/g, " ").slice(0, 150))
    add("the paid overview shows the renewal date and the 100 GB allowance", /Renews\s+\w{3} \d{1,2}, \d{4}/.test(paid) && /of 100 GB used/.test(paid))
    const billing = await overview()
    add("the test webhook made the subscription active", billing.state === "active" && billing.product_code === "pro-month", `state=${billing.state} product=${billing.product_code}`)
  }

  // Nothing is clicked on a Stripe page unless it is the test-mode portal.
  async function enteredPortal() {
    await page.waitForURL((u) => u.hostname === "billing.stripe.com", { timeout: 90000, waitUntil: "commit" })
    const sandbox = await page.getByText("Sandbox", { exact: true }).first().waitFor({ timeout: 60000 }).then(() => true, () => false)
    if (!sandbox || !testPortalURL(page.url())) throw new Error("refusing to continue: not a Stripe test-mode customer portal")
  }

  async function openPortal() {
    await openPlan()
    await main_().getByRole("button", { name: "Manage billing" }).click({ timeout: 60000 })
    await enteredPortal()
  }

  async function teamCheckout(id, seats) {
    await open("/team", () => main_().getByRole("button", { name: "Create team" }).first().waitFor({ timeout: 90000 }))
    const dialog = page.getByRole("dialog")
    for (let i = 0; i < 5 && !(await dialog.count()); i++) {
      await main_().getByRole("button", { name: "Create team" }).first().click()
      await page.waitForTimeout(2000)
    }
    await dialog.locator("#team-id").fill(id)
    for (let i = 1; i < seats; i++) await dialog.getByRole("button", { name: "More seats" }).click()
    const pay = dialog.getByRole("button", { name: "Continue to payment" })
    add("a new team needs the purchase terms ticked before payment", await pay.isDisabled())
    await dialog.getByRole("checkbox").first().click()
    await pay.click()
    await page.waitForURL((u) => u.hostname === "checkout.stripe.com", { timeout: 90000, waitUntil: "commit" })
    add("Continue to payment opens a Stripe test-mode Checkout", /\/pay\/cs_test_/.test(page.url()))
    await payTestCheckout(page) // refuses anything but a sandbox session
    await page.waitForURL((u) => u.href.startsWith(origin + "/team"), { timeout: 180000, waitUntil: "commit" })
    add("Checkout returns to the console's team page", true)
    // the team exists when Stripe's test webhook reaches the server
    let billing
    for (let i = 0; i < 60 && !(billing?.state === "active"); i++) {
      const reply = await page.request.get(`${origin}/api/v1/billing/workspaces/teams/${id}`).catch(() => undefined)
      billing = reply?.ok() ? (await reply.json()).data : undefined
      if (billing?.state !== "active") await page.waitForTimeout(3000)
    }
    add("the test webhook made the team and its subscription active", billing?.state === "active" && billing.seats_bought === seats, `state=${billing?.state} seats=${billing?.seats_bought}`)
    await context.addCookies([{ name: "pb-workspace", value: id, url: origin }])
    await openPlan()
    const paid = await settled(main_(), (t) => /Plan\s+Team · \$\d+\/month/.test(t) && /of \d+ GB used/.test(t))
    add("the paid team overview shows Team, monthly, its renewal and the per-seat allowance", /Plan\s+Team · \$\d+\/month/.test(paid) && /Renews\s+\w{3} \d{1,2}, \d{4}/.test(paid) && paid.includes(`of ${seats * 100} GB used`), paid.replace(/\s+/g, " ").slice(0, 200))
  }

  async function seats(wanted) {
    const before = await overview()
    const more = wanted > before.seats_bought
    await openPlan()
    const button = main_().getByRole("button", { name: more ? "Add a seat" : "Remove a seat" })
    await button.waitFor({ timeout: 60000 })
    for (let i = 0; i < Math.abs(wanted - before.seats_bought); i++) await button.click()
    const text = await settled(main_(), (t) => /now · then/.test(t))
    const preview = /(-?\$[\d,.]+) now · then (-?\$[\d,.]+) on (\w{3} \d{1,2})/.exec(text.replace(/\s+/g, " "))
    add("the change shows what it costs now and from which date", Boolean(preview), preview?.[0] ?? text.replace(/\s+/g, " ").slice(0, 200))
    if (preview) add(more ? "an added seat is charged now" : "a removed seat charges nothing now", more ? amount(preview[1]) > 0 : amount(preview[1]) <= 0, preview[0])
    await main_().getByRole("button", { name: more ? "Buy seats" : "Remove seats", exact: true }).click()
    // an added seat is there at once; a removed one stays paid and usable until the billing date, where the lower count is scheduled
    const done = (b) => (more ? b.seats_bought === wanted : b.scheduled_seats === wanted)
    let after = before
    for (let i = 0; i < 40 && !done(after); i++) {
      await page.waitForTimeout(3000)
      after = await overview()
    }
    if (more) {
      add(`the team has ${wanted} seats`, done(after), `seats ${before.seats_bought} -> ${after.seats_bought}`)
    } else {
      add(`the removal is scheduled for the billing date: ${wanted} seats from then`, done(after) && after.scheduled_seats_at === before.period_end,
        `scheduled ${after.scheduled_seats} at ${after.scheduled_seats_at}, billing date ${before.period_end}`)
      add("the removed seat stays paid until then", after.seats_bought === before.seats_bought && after.allowance_bytes === before.allowance_bytes, `seats ${after.seats_bought}, allowance ${after.allowance_bytes}`)
    }
    add("the billing date did not move", after.period_end === before.period_end && after.period_start === before.period_start, `${before.period_end} -> ${after.period_end}`)
    if (more) {
      await openPlan()
      const shown = await settled(main_(), (t) => new RegExp(`${wanted} seats?`).test(t))
      add("the billing page shows the new seat count", new RegExp(`${wanted} seats?`).test(shown))
    }
  }

  async function invoices() {
    await openPortal()
    const text = await settled(page.locator("body"), (t) => /Invoice history/i.test(t))
    await page.waitForTimeout(3000)
    const history = (await page.locator("body").innerText()).split(/invoice history/i)[1] ?? ""
    const paid = (history.match(/\bPaid\b/g) ?? []).length
    add("invoices", paid > 0, `${paid} paid: ${history.replace(/\s+/g, " ").slice(0, 180)}`)
    steps.at(-1).paid = paid
  }

  async function portal(action) {
    await openPlan()
    const label = { month: /^Switch to monthly/, year: /^Switch to yearly/, cancel: "Cancel plan", keep: "Keep plan" }[action]
    await main_().getByRole("button", { name: label }).click({ timeout: 60000 })
    await enteredPortal()
    add(`'${action}' opens Stripe's test-mode portal`, true)
    const click = (locator) => locator.first().click({ timeout: 60000 })
    if (action === "month" || action === "year") {
      await click(page.locator('[data-test="update-subscription"]'))
      await click(page.locator(`[data-testid^="pricing-table-interval-${action}"]`))
      await click(page.locator('[data-testid="continue-button"]'))
      await click(page.locator('[data-testid="confirm"]'))
    } else if (action === "cancel") {
      await click(page.locator('[data-test="cancel-subscription"]'))
      await click(page.locator('[data-testid="confirm"]').or(page.getByRole("button", { name: /^Cancel subscription$/ })))
    } else {
      await click(page.getByRole("link", { name: /renew subscription|don.t cancel/i }).or(page.getByRole("button", { name: /renew subscription|don.t cancel/i })))
      await click(page.locator('[data-testid="confirm"]').or(page.getByRole("button", { name: /^Renew/ })))
    }
    await page.waitForTimeout(8000)
    await openPlan()
    const want = {
      month: /Plan\s+Pro · \$8\/month/,
      year: /Plan\s+Pro · \$80\/year/,
      cancel: /Ends\s+\w{3} \d{1,2}, \d{4}[\s\S]*Keep plan/,
      keep: /Renews\s+\w{3} \d{1,2}, \d{4}[\s\S]*Cancel plan/,
    }[action]
    const text = await settled(main_(), (t) => want.test(t))
    add(`the console shows the plan after '${action}'`, want.test(text), text.replace(/\s+/g, " ").slice(0, 200))
  }

  try {
    if (command === "card") await card(argument)
    else if (command === "checkout") await checkout()
    else if (command === "portal") await portal(argument)
    else if (command === "team-checkout") await teamCheckout(argument, Number(extra))
    else if (command === "seats") await seats(Number(argument))
    else if (command === "invoices") await invoices()
    else throw new Error("usage: card free|pro|throttled | checkout | portal month|year|cancel|keep | team-checkout TEAM SEATS | seats N | invoices")
  } catch (error) {
    const where = page.url().split("?")[0].replace(origin, "").slice(0, 60)
    add(`${command} ${argument ?? ""} completed`, false, `${String(error?.message ?? error).split("\n")[0].slice(0, 160)} (on ${where})`)
    if (process.env.BILLING_SHOT) await page.screenshot({ path: process.env.BILLING_SHOT, fullPage: true }).catch(() => {})
  } finally {
    await browser.close()
  }
  console.log(JSON.stringify({ steps }))
  process.exit(steps.length && steps.every((s) => s.ok) ? 0 : 1)
}

if (process.argv[1] === fileURLToPath(import.meta.url)) await main(process.argv.slice(2))
