// Shared helpers of the live mode of check-dns-status.mjs and check-dns-binding-badges.mjs: sign a stored test account in to a real
// console and read one real domain's DNS badges. Read-only: it only navigates, opens the domain's panel and reads the page; it never
// clicks a button that changes anything and never calls the API with anything but what the console itself does.
//
// env (live mode is on when LIVE=1 or ORIGIN is set):
//   ORIGIN          the console origin, e.g. https://console.example.test
//   DOMAIN          the real test domain's hostname as listed on the Network page (Domains)
//   STATE           a Playwright storage state of a signed-in test account (tools/device-smoke/console_session.mjs writes one), or
//   EMAIL + PASSWORD_FILE   the test account's email and the file holding its WorkOS password (never printed); the helper signs in
//   EXPECT_HEALTH   optional: healthy | wrong_address | missing | checking; the badge must be exactly this
//   PW_CHROMIUM / PW_MODULE as for the stub mode
import { readFileSync } from "node:fs"

export const liveMode = () => process.env.LIVE === "1" || Boolean(process.env.ORIGIN)
export const HEALTHS = ["healthy", "wrong_address", "missing", "checking"]
export const LABELS = { healthy: "Healthy", wrong_address: "Wrong address", missing: "Missing", checking: "Checking" }
const PILLS = ["verified", "pending", "wrong_address", "missing"]
export const squash = (text) => text.replace(/\s+/g, " ")

export const liveConfig = () => {
  const { ORIGIN: origin, DOMAIN: domain, STATE: state, EMAIL: email, PASSWORD_FILE: passwordFile, EXPECT_HEALTH: expect } = process.env
  if (!origin || !domain) throw new Error("live mode needs ORIGIN and DOMAIN; see the header of scripts/dns-live.mjs")
  if (!state && !(email && passwordFile)) throw new Error("live mode needs STATE, or EMAIL and PASSWORD_FILE of the test account")
  if (expect && !HEALTHS.includes(expect)) throw new Error(`EXPECT_HEALTH must be one of ${HEALTHS.join(", ")}`)
  return { origin: origin.replace(/\/+$/, ""), domain: domain.toLowerCase(), state, email, passwordFile, expect }
}

export const launch = async (playwright) => {
  const chromium = playwright.chromium ?? playwright.default.chromium
  return chromium.launch(process.env.PW_CHROMIUM ? { executablePath: process.env.PW_CHROMIUM } : {})
}

// A context signed in as the test account. With STATE the stored session is used; otherwise the WorkOS password sign-in runs once
// (email, Enter, password, Enter, the same steps as tools/device-smoke/console_session.mjs) and the context keeps the cookies.
export const signedInContext = async (browser, cfg, viewport) => {
  const context = await browser.newContext({ viewport, ...(cfg.state ? { storageState: cfg.state } : {}) })
  if (cfg.state) return context
  const page = await context.newPage()
  await page.goto(cfg.origin + "/")
  await page.waitForTimeout(3000)
  const link = page.getByRole("link", { name: /sign in/i })
  if (await link.count()) await link.first().click()
  await page.waitForSelector('input[name="email"]', { timeout: 30_000 })
  await page.fill('input[name="email"]', cfg.email)
  await page.keyboard.press("Enter")
  await page.waitForSelector('input[type="password"]', { timeout: 30_000 })
  await page.fill('input[type="password"]', readFileSync(cfg.passwordFile, "utf8").trim())
  await page.keyboard.press("Enter")
  await page.waitForURL((url) => url.origin === cfg.origin && !/\/(auth|sign-?in)/.test(url.pathname), { timeout: 60_000 })
  await page.close()
  return context
}

// Opens the Network page, then the panel of the real domain; returns {page, panel, why}; panel is null when it did not open.
export const openDomainPanel = async (context, cfg) => {
  const page = await context.newPage()
  const seen = []
  page.on("pageerror", (error) => seen.push(`pageerror ${error.message.slice(0, 200)}`))
  await page.goto(`${cfg.origin}/network`, { timeout: 120_000 })
  const clicked = await page.getByRole("button", { name: cfg.domain, exact: true }).click({ timeout: 60_000 }).then(() => "", (error) => error.message.split("\n")[0])
  const panel = await page.waitForSelector('[data-slot="sheet-content"]', { timeout: clicked ? 1_000 : 60_000 }).catch(() => null)
  const why = panel ? "" : `${clicked || "clicked, no panel"} at ${page.url()}: ${squash((await page.locator("body").innerText().catch(() => "")).slice(0, 200))} ${seen.join(", ")}`
  return { page, panel, why }
}

// What the panel says about the real domain's DNS: the header badge, each record's pill, the problem notes and what uses the domain.
export const readPanel = async (panel) => ({
  text: squash(await panel.innerText()),
  health: await panel.$eval("[data-dns-health]", (e) => e.getAttribute("data-dns-health")).catch(() => null),
  healthText: await panel.$eval("[data-dns-health]", (e) => e.innerText.replace(/\s+/g, " ")).catch(() => ""),
  pills: await panel.$$eval("[data-dns-record] [data-dns-status]", (e) => e.map((p) => p.getAttribute("data-dns-status"))),
  notes: await panel.$$eval("[data-dns-note]", (e) => e.map((n) => n.innerText.replace(/\s+/g, " "))),
  records: await panel.$$eval("[data-dns-record]", (e) => e.map((r) => ({ purpose: r.getAttribute("data-dns-record"), status: r.querySelector("[data-dns-status]")?.getAttribute("data-dns-status") ?? null }))),
  copies: await panel.$$eval("button[aria-label^='Copy']", (b) => b.length),
  tunnelLinks: await panel.$$eval('a[href^="/tunnels/"]', (a) => [...new Set(a.map((x) => x.getAttribute("href")))]),
  usesPreview: (await panel.$$('a[href="/previews"]')).length > 0,
})

// The header badge must agree with the record pills: the real server decides both, the console must not contradict itself.
export const healthAgreesWithPills = (health, pills) => ({
  healthy: pills.length > 0 && pills.every((p) => p === "verified"),
  wrong_address: pills.includes("wrong_address"),
  missing: pills.includes("missing") && !pills.includes("wrong_address"),
  checking: pills.includes("pending"),
})[health] ?? false
export const knownPills = (pills) => pills.every((p) => PILLS.includes(p))
