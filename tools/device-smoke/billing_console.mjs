// The live console's billing screens and its Stripe TEST-MODE round trips, for billing_checks.py.
// usage: ORIGIN=<console> STATE=<storage state of a THROWAWAY account> PW_CHROMIUM=<chromium> [PW_MODULE=...] node billing_console.mjs
//   card free|pro|throttled     F13-06: the plan page and the usage page's allowance card agree with the billing and usage APIs;
//                               console/scripts/check-usage-paths.js (Direct/Relay/Edge cards) runs on the same page
//   checkout                    F13-04C: Upgrade (Pro, monthly) on the console, Stripe test card, back to the paid overview
//   portal month|year           Manage billing: switch the billing interval in Stripe's portal, back to the console
//   portal cancel|keep          Cancel plan / Keep plan in Stripe's portal, back to the console
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
  const [command, argument] = argv
  const origin = process.env.ORIGIN
  const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
  const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
  const page = await (await browser.newContext({ storageState: process.env.STATE, viewport: { width: 1280, height: 1600 } })).newPage()
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
  const overview = () => get("billing/workspaces/personal")
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

  async function portal(action) {
    await openPlan()
    const label = { month: /^Switch to monthly/, year: /^Switch to yearly/, cancel: "Cancel plan", keep: "Keep plan" }[action]
    await main_().getByRole("button", { name: label }).click({ timeout: 60000 })
    await page.waitForURL((u) => u.hostname === "billing.stripe.com", { timeout: 90000, waitUntil: "commit" })
    const sandbox = await page.getByText("Sandbox", { exact: true }).first().waitFor({ timeout: 60000 }).then(() => true, () => false)
    if (!sandbox || !testPortalURL(page.url())) throw new Error("refusing to continue: not a Stripe test-mode customer portal")
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
    else throw new Error("usage: card free|pro|throttled | checkout | portal month|year|cancel|keep")
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
