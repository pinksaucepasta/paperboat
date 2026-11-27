// Operator announcements as a person sees them, for announcements made for one throwaway account only (the caller makes
// and retracts them through the operator API): the ribbon above every page, the bell's unread notice, read and dismiss
// kept by the server, and nothing for another account.
// usage: ORIGIN=... STATE=<the audience> OTHER_STATE=<another account> BANNER=<title> NOTICE=<title> [PW_MODULE=...]
//        node console_announcements.mjs shown|gone
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const origin = process.env.ORIGIN
const { BANNER: banner, NOTICE: notice } = process.env
const mode = process.argv[2]
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
const open = async (state) => (await browser.newContext({ storageState: state, viewport: { width: 1280, height: 900 } })).newPage()
const until = async (probe, ms = 90000) => {
  for (const end = Date.now() + ms; Date.now() < end; await new Promise((r) => setTimeout(r, 1000))) {
    const found = await probe().catch(() => false)
    if (found) return found
  }
  return false
}
const api = async (page) => JSON.stringify((await (await page.context().request.get(`${origin}/api/v1/announcements`)).json()).data)
const ribbon = (page) => page.locator("[data-announcement-banner]", { hasText: banner }).filter({ visible: true }).first()
const load = async (page, path) => {
  await page.goto(origin + path, { waitUntil: "domcontentloaded", timeout: 120000 })
  await page.waitForTimeout(5000)
}
try {
  const page = await open(process.env.STATE)
  if (mode === "gone") {
    await load(page, "/")
    const data = await api(page)
    log(!data.includes(banner) && !data.includes(notice), "retracted announcements are gone from the server's answer")
    log((await ribbon(page).count()) === 0, "and from the page")
  } else {
    await load(page, "/")
    log(Boolean(await until(async () => (await ribbon(page).count()) > 0)), "the ribbon is above the page")
    for (const path of ["/previews", "/network"]) {
      await load(page, path)
      log(Boolean(await until(async () => (await ribbon(page).count()) > 0, 30000)), `the ribbon is above ${path} too`)
    }
    const bell = page.locator("header button, [data-slot=sidebar-inset] button").filter({ has: page.locator("svg") }).and(page.getByRole("button", { name: /notification|notice|request|unread|waiting|new/i })).first()
    log(Boolean(await until(async () => (await bell.count()) > 0, 30000)), "the header bell shows", await bell.getAttribute("aria-label").catch(() => ""))
    await bell.click({ timeout: 15000 })
    log(Boolean(await until(async () => (await page.getByText(notice).filter({ visible: true }).count()) > 0, 30000)), "the bell lists the notice")
    await page.keyboard.press("Escape")
    await ribbon(page).getByRole("button", { name: /dismiss|close/i }).click({ timeout: 15000 })
    log(Boolean(await until(async () => (await ribbon(page).count()) === 0, 30000)), "Dismiss removes the ribbon")
    await load(page, "/")
    log((await ribbon(page).count()) === 0, "it stays dismissed after a reload (the server keeps it)")
    const other = await open(process.env.OTHER_STATE)
    await load(other, "/")
    const theirs = await api(other)
    log(!theirs.includes(banner) && !theirs.includes(notice) && (await ribbon(other).count()) === 0, "another account sees neither")
  }
} catch (error) {
  log(false, "announcement check stopped", String(error.message).split("\n")[0].slice(0, 200))
} finally {
  await browser.close()
}
process.exit(failed ? 1 : 0)
