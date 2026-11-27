// The live console's billing screens and its Stripe TEST-MODE round trips, for billing_checks.py.
// usage: ORIGIN=<console> STATE=<storage state of a THROWAWAY account> PW_CHROMIUM=<chromium> [PW_MODULE=...] node billing_console.mjs
//   card free|pro|throttled     F13-06: the plan page and the usage page's allowance card agree with the billing and usage APIs;
//                               console/scripts/check-usage-paths.js (Direct/Relay/Edge cards) runs on the same page
//   checkout                    F13-04C: Upgrade (Pro, monthly) on the console, Stripe test card, back to the paid overview. First a
//                               Checkout is left unpaid (L38): the next must open under a new operation ID and end the abandoned one
//   seat-note BOUGHT THEN cancelling|renewing   L38: the Seats section's line about a scheduled removal
//   portal month|year           Manage billing: switch the billing interval in Stripe's portal, back to the console
//   portal cancel|keep          Cancel plan / Keep plan in Stripe's portal, back to the console
//   team-checkout TEAM SEATS    Create team (monthly) on the console, pay on test-mode Checkout, the team's paid overview
//   seats N                     the team's billing page: set N seats, read what it costs now, buy or remove
//   declined                    F13-04: Upgrade with a declined test card (workspace stays Free), then the good card on the same Checkout
//   payment-state grace|paused|none   what the billing page says about a failed renewal (F13-04G)
//   topup UNITS                 F13-04T: buy UNITS x 100 GB on the console, test-mode Checkout; credited once
//   invoices                    the Stripe portal's invoice history; the step "invoices" carries the number of paid ones in "paid"
// With BILLING_TEAM=<team id> the plan, seats, portal and invoice commands act on that team's workspace instead of the personal one.
// Prints one JSON line {"steps":[{"step","ok","detail"}]}: every step is tried, none stops the rest. Exit 1 when one failed.
// Stripe pages are refused unless they are test-mode ones (stripe_pay.mjs; the portal: a test_ session AND the "Sandbox" badge).
import { readFileSync } from "node:fs"
import { fileURLToPath } from "node:url"
import { payTestCheckout, testCheckoutURL } from "./stripe_pay.mjs"

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

export const sessionOf = (url) => /cs_test_[A-Za-z0-9]+/.exec(url ?? "")?.[0]
// What Stripe says on a Checkout session that was ended; any other wording fails the step and is quoted in its detail.
export const expiredCheckout = (text) => /expired|timed out|no longer (active|available|valid)|not active|all done|could not be found/i.test(text)

// The Seats section's line about a scheduled removal, dated with the billing date the same page shows: [ok, what it shows].
export function seatNote(text, bought, then, cancelling) {
  const flat = text.replace(/\s+/g, " ")
  const date = /(?:Renews|Ends) (\w{3} \d{1,2}, \d{4})/.exec(flat)?.[1]
  const note = /(\d+) seats? until (\w{3} \d{1,2}, \d{4}), then (\d+)( if you keep the plan)?\./.exec(flat)
  const ok = Boolean(date && note) && Number(note[1]) === bought && note[2] === date && Number(note[3]) === then && Boolean(note[4]) === cancelling
  return [ok, `${note?.[0] ?? `no such line; Seats shows: ${/Seats (.{0,90})/.exec(flat)?.[1] ?? "nothing"}`} (billing date ${date})`]
}

// Everything below the portal's "Invoice history" heading, and the invoices in it marked Paid.
export const historyOf = (text) => text.split(/invoice history/i).slice(1).join(" ")
export const paidRows = (history) => (history.match(/\bPaid\b/g) ?? []).length

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
  const [command, argument, extra, last] = argv
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

  // The billing page, freshly loaded (so a new operation ID), with the purchase terms ready to tick.
  async function upgradeOffer() {
    await openPlan()
    const consent = main_().getByRole("checkbox").first()
    await consent.waitFor({ timeout: 60000 })
    for (let i = 0; i < 30 && !(await consent.isEnabled()); i++) await page.waitForTimeout(1000)
    return { consent, offer: main_().getByRole("button", { name: "Upgrade", exact: true }) }
  }
  const atStripe = () => page.waitForURL((u) => u.hostname === "checkout.stripe.com", { timeout: 90000, waitUntil: "commit" })

  // L38: a Checkout left unpaid must not block the next one, and must not stay payable beside it. If the next one is refused, the
  // abandoned session (still open) is paid instead so the rest of the row runs; the failed step stays failed.
  async function abandonThenUpgrade() {
    const operations = []
    let answered = ""
    const isCheckout = (r) => r.method() === "POST" && new URL(r.url()).pathname === "/api/v1/billing/workspaces/personal/checkout"
    page.on("request", (r) => isCheckout(r) && operations.push(r.headers()["idempotency-key"]))
    page.on("response", (r) => isCheckout(r.request()) && (answered = `HTTP ${r.status()}`))
    const first = await upgradeOffer()
    add("Upgrade is disabled until the purchase terms are ticked", await first.offer.isDisabled())
    await first.consent.click()
    add("the offered plan is Pro at $8/month", /Pro\s+\$8\/month/.test(await main_().innerText()))
    await first.offer.click()
    await atStripe()
    const abandoned = page.url()
    add("Upgrade opens a Stripe test-mode Checkout", testCheckoutURL(abandoned))
    const again = await upgradeOffer() // the first Checkout is left unpaid
    await again.consent.click()
    await again.offer.click()
    const opened = await atStripe().then(() => true, () => false)
    const fresh = opened && testCheckoutURL(page.url()) && sessionOf(page.url()) !== sessionOf(abandoned)
    const refusal = opened ? "" : (/[^.\n]*(billing operation is pending|operation ID)[^\n]*/i.exec(await main_().innerText().catch(() => ""))?.[0] ?? "")
    add("L38: with a Checkout left unpaid, a new one under a new operation ID opens its own Checkout",
      fresh && operations.length === 2 && Boolean(operations[0]) && operations[0] !== operations[1],
      `${operations.length} requests, ${new Set(operations).size} operation IDs, last ${answered}, new session ${fresh} ${refusal}`)
    if (!fresh) {
      await page.goto(abandoned, { waitUntil: "domcontentloaded", timeout: 60000 })
      return
    }
    const old = await context.newPage()
    await old.goto(abandoned, { waitUntil: "domcontentloaded", timeout: 60000 })
    let shown = ""
    let payable = false
    for (let i = 0; i < 60 && !payable && !expiredCheckout(shown); i++) {
      await old.waitForTimeout(1000)
      shown = await old.locator("body").innerText().catch(() => "")
      for (const frame of old.frames()) payable ||= (await frame.getByPlaceholder("Card number", { exact: true }).count().catch(() => 0)) > 0
    }
    add("L38: the abandoned Checkout can no longer be paid", !payable && expiredCheckout(shown), `card form shown: ${payable}; page: ${shown.replace(/\s+/g, " ").slice(0, 150)}`)
    await old.close()
  }

  async function checkout() {
    await abandonThenUpgrade()
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

  // A card the issuer declines (Stripe's test number 4000 0000 0000 0002) leaves the workspace Free and the Checkout payable.
  async function declined() {
    const offer = await upgradeOffer()
    await offer.consent.click()
    await offer.offer.click()
    await atStripe()
    const refusal = await payTestCheckout(page, "4000000000000002").then(() => "the payment was accepted", (error) => String(error.message))
    add("Checkout declines the card and says so", /declined/i.test(refusal), refusal)
    add("the buyer is still on the same Checkout", testCheckoutURL(page.url()), page.url().split("?")[0].slice(0, 60))
    await page.waitForTimeout(15000) // a webhook of the failed attempt, if any, has arrived
    const billing = await overview()
    add("after the declined card the workspace is still Free", billing.state === "free" && !billing.product_code, `state=${billing.state} product=${billing.product_code}`)
    await payTestCheckout(page)
    await page.waitForURL((u) => u.href.startsWith(origin + "/billing"), { timeout: 180000, waitUntil: "commit" })
    const paid = await settled(main_(), (t) => /Plan\s+Pro · \$8\/month/.test(t) && !/Confirming payment/.test(t))
    add("a good card on the same Checkout recovers: the paid overview shows Pro · $8/month", /Plan\s+Pro · \$8\/month/.test(paid), paid.replace(/\s+/g, " ").slice(0, 150))
    const after = await overview()
    add("no failed-payment state remains after the recovery", after.state === "active" && !after.renewal_grace_until && !/Payment failed/.test(paid), `state=${after.state} grace=${after.renewal_grace_until}`)
  }

  // F13-04T: a one-time top-up of UNITS x 100 GB bought on the console, paid on test-mode Checkout.
  async function topup(units) {
    const before = await overview()
    await openPlan()
    const buy = main_().getByRole("button", { name: "Buy", exact: true })
    await buy.waitFor({ timeout: 60000 })
    const consent = main_().getByRole("checkbox").last()
    add("Buy is disabled until the purchase terms are ticked", await buy.isDisabled())
    for (let i = 1; i < units; i++) await main_().getByRole("button", { name: "100 GB more" }).click()
    const amountShown = await main_().getByRole("spinbutton", { name: "Gigabytes" }).inputValue()
    add(`the form offers ${units * 100} GB`, Number(amountShown) === units * 100, amountShown)
    await consent.click()
    await buy.click()
    await atStripe()
    add("Buy opens a Stripe test-mode Checkout", testCheckoutURL(page.url()))
    await payTestCheckout(page)
    await page.waitForURL((u) => u.href.startsWith(origin + "/billing"), { timeout: 180000, waitUntil: "commit" })
    let after = before
    for (let i = 0; i < 40 && !((after.topup_bytes_purchased ?? 0) > (before.topup_bytes_purchased ?? 0)); i++) {
      await page.waitForTimeout(3000)
      after = await overview()
    }
    const gained = (after.topup_bytes_purchased ?? 0) - (before.topup_bytes_purchased ?? 0)
    add(`the paid top-up credits ${units * 100} GB once`, gained === units * 1e11 && after.topup_bytes_remaining - (before.topup_bytes_remaining ?? 0) === units * 1e11, `purchased +${gained}, remaining ${after.topup_bytes_remaining}`)
    add("the plan, its allowance and its billing dates are unchanged", after.product_code === before.product_code && after.allowance_bytes === before.allowance_bytes && after.period_end === before.period_end && after.allowance_period_end === before.allowance_period_end, `${after.product_code} ${after.allowance_bytes} ${after.period_end}`)
    await openPlan()
    const shown = await settled(main_(), (t) => /extra left/.test(t))
    add("the billing page shows the extra bandwidth left", /[\d.]+ [GT]B extra left/.test(shown), /([\d.]+ \S?B) extra left/.exec(shown.replace(/\s+/g, " "))?.[0] ?? shown.replace(/\s+/g, " ").slice(0, 200))
    await page.waitForTimeout(10000) // a replayed webhook must not credit twice
    const later = await overview()
    add("no second credit arrives for the same payment", later.topup_bytes_purchased === after.topup_bytes_purchased, `${later.topup_bytes_purchased}`)
  }

  // What the billing page says after a renewal payment failed: within the grace period (paid features stay), after it (paused), or
  // nothing once the payment succeeded.
  async function paymentState(expected) {
    await openPlan()
    const want = { grace: /Payment failed\. Paid features stay until \w{3} \d{1,2}, \d{4}\./, paused: /Payment failed\. Paid features are paused until payment succeeds\./, none: /Plan\s+Pro · \$8\/month/ }[expected]
    const done = (t) => want.test(t) && (expected !== "none" || !/Payment failed|Payment overdue/.test(t))
    const text = await settled(main_(), done)
    add(`the billing page shows the '${expected}' payment state`, done(text), text.replace(/\s+/g, " ").slice(0, 220))
    const update = await main_().getByRole("button", { name: "Update payment method" }).count()
    add(expected === "none" ? "no 'Update payment method' prompt remains" : "it offers 'Update payment method'", expected === "none" ? update === 0 : update === 1, `${update} buttons`)
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
    if (preview) {
      add(more ? "an added seat is charged now" : "a removed seat charges nothing now", more ? amount(preview[1]) > 0 : amount(preview[1]) === 0, preview[0])
      const next = (wanted * before.product.unit_amount) / 100
      add(`the next invoice is the price of ${wanted} seats`, amount(preview[2]) === next, `shows ${preview[2]}, ${wanted} seats cost $${next}`)
    }
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
    } else {
      // what the buyer is looking at when the removal lands, before any reload
      await expectNote("L38: right after Remove seats, without a reload", before.seats_bought, wanted, false)
      await openPlan()
      await expectNote("L38: after a reload", before.seats_bought, wanted, false)
    }
  }

  async function expectNote(when, bought, then, cancelling) {
    const text = await settled(main_(), (t) => seatNote(t, bought, then, cancelling)[0])
    const [ok, detail] = seatNote(text, bought, then, cancelling)
    add(`${when}, the Seats section reads "${bought} seats until <billing date>, then ${then}${cancelling ? " if you keep the plan" : ""}."`, ok, detail)
  }

  async function invoices() {
    await openPortal()
    await settled(page.locator("body"), (t) => /invoice history[\s\S]*\bPaid\b/i.test(t))
    await page.waitForTimeout(5000) // the rows arrive together; a late one would otherwise be missed
    const history = historyOf(await page.locator("body").innerText())
    const paid = paidRows(history)
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
      month: /Plan\s+(Pro · \$8|Team · \$[\d,]+)\/month/,
      year: /Plan\s+(Pro · \$80|Team · \$[\d,]+)\/year/,
      cancel: /Ends\s+\w{3} \d{1,2}, \d{4}[\s\S]*Keep plan/,
      keep: /Renews\s+\w{3} \d{1,2}, \d{4}[\s\S]*Cancel plan/,
    }[action]
    const text = await settled(main_(), (t) => want.test(t))
    add(`the console shows the plan after '${action}'`, want.test(text), text.replace(/\s+/g, " ").slice(0, 200))
  }

  try {
    if (command === "card") await card(argument)
    else if (command === "checkout") await checkout()
    else if (command === "declined") await declined()
    else if (command === "payment-state") await paymentState(argument)
    else if (command === "topup") await topup(Number(argument))
    else if (command === "portal") await portal(argument)
    else if (command === "team-checkout") await teamCheckout(argument, Number(extra))
    else if (command === "seats") await seats(Number(argument))
    else if (command === "invoices") await invoices()
    else if (command === "seat-note") {
      await openPlan()
      await expectNote(`L38: ${last}`, Number(argument), Number(extra), last === "cancelling")
    } else throw new Error("usage: card free|pro|throttled | checkout | portal month|year|cancel|keep | team-checkout TEAM SEATS | seats N | declined | topup UNITS | payment-state grace|paused|none | seat-note BOUGHT THEN cancelling|renewing | invoices")
  } catch (error) {
    const where = page.url().split("?")[0].replace(origin, "").slice(0, 60)
    const why = String(error?.message ?? error).split("\n")[0].replace(/(https?:\/\/[^\s?"']+)\?[^\s"']*/g, "$1").slice(0, 160)
    add(`${command} ${argument ?? ""} completed`, false, `${why} (on ${where})`)
    if (process.env.BILLING_SHOT) await page.screenshot({ path: process.env.BILLING_SHOT, fullPage: true }).catch(() => {})
  } finally {
    await browser.close()
  }
  console.log(JSON.stringify({ steps }))
  process.exit(steps.length && steps.every((s) => s.ok) ? 0 : 1)
}

if (process.argv[1] === fileURLToPath(import.meta.url)) await main(process.argv.slice(2))
