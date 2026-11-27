// The /access-login page ("Signing in"): a private preview opened in a browser goes to the console and, for the owner's
// signed-in browser, comes back to the page; a signed-out browser is asked to sign in; another account sees what a name
// that does not exist would show. STATE is the owner, STRANGER_STATE another account.
// usage: ORIGIN=... STATE=... STRANGER_STATE=... MARKER=<text the app serves> [PW_MODULE=...] node console_access_login.mjs <preview url>
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const origin = process.env.ORIGIN
const url = process.argv[2]
const marker = process.env.MARKER
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
async function visit(state) {
  const context = await browser.newContext(state ? { storageState: state } : {})
  const page = await context.newPage()
  const seen = []
  page.on("framenavigated", (frame) => frame === page.mainFrame() && seen.push(new URL(frame.url()).host + new URL(frame.url()).pathname))
  let last = 0
  page.on("response", (r) => {
    if (r.request().isNavigationRequest() && r.frame() === page.mainFrame()) last = r.status()
  })
  await page.goto(url, { waitUntil: "domcontentloaded", timeout: 120000 }).catch(() => {})
  let text = ""
  for (let i = 0; i < 90; i++) {
    text = await page.locator("body").innerText().catch(() => "")
    const at = new URL(page.url())
    if (text.includes(marker) || (state && at.host === new URL(url).host && last >= 400)) break
    if (!state && ((at.host !== new URL(url).host && at.host !== new URL(origin).host) || (await page.locator('input[name="email"]').count()))) break
    await page.waitForTimeout(1000)
  }
  return { context, page, seen, text, status: last }
}
try {
  const owner = await visit(process.env.STATE)
  log(owner.text.includes(marker), "the owner's browser lands on the private page", `HTTP ${owner.status}`)
  log(new URL(owner.page.url()).host === new URL(url).host, "back on the preview's own address", new URL(owner.page.url()).host)
  log(owner.seen.some((at) => at.startsWith(new URL(origin).host) && /access/.test(at)), "by way of the console's Signing in page", owner.seen.join(" → "))
  log(!/handoff|token/.test(owner.page.url()), "no handoff or token is left in the address")
  const nobody = await visit(null)
  log(!nobody.text.includes(marker), "a signed-out browser does not get the page")
  log((await nobody.page.locator('input[name="email"]').count()) > 0 || /sign in/i.test(nobody.text), "it is asked to sign in", new URL(nobody.page.url()).host + new URL(nobody.page.url()).pathname)
  const stranger = await visit(process.env.STRANGER_STATE)
  log(!stranger.text.includes(marker) && stranger.status === 404, "another account gets 404, as for a name that does not exist", `HTTP ${stranger.status}`)
} catch (error) {
  log(false, "access login check stopped", String(error.message).split("\n")[0].slice(0, 200))
} finally {
  await browser.close()
}
process.exit(failed ? 1 : 0)
