// Ends the console session of a stored sign-in (STATE, mode 600) the way the Sign out menu item does: the server session is revoked and
// the WorkOS session is ended. Then proves it: GET /api/v1/me must answer 401. Prints nothing secret.
// usage: STATE=<storage state> ORIGIN=<console origin> PW_CHROMIUM=<chromium> [PW_MODULE=<playwright-core index.mjs>] node console_signout.mjs
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
const context = await browser.newContext({ storageState: process.env.STATE })
const page = await context.newPage()
// The console signs out by posting its hidden /auth/sign-out form (components/user-menu.tsx); the browser then follows the
// redirects through WorkOS's logout and back.
await page.goto(process.env.ORIGIN + "/", { waitUntil: "domcontentloaded", timeout: 60000 })
await page.waitForSelector('form[action="/auth/sign-out"]', { state: "attached", timeout: 30000 })
await Promise.all([
  page.waitForURL((url) => !url.pathname.startsWith("/auth/sign-out"), { timeout: 60000 }).catch(() => {}),
  page.evaluate(() => document.querySelector('form[action="/auth/sign-out"]').requestSubmit()),
])
await page.waitForLoadState("networkidle").catch(() => {})
await page.waitForTimeout(3000)
const me = await context.request.get(process.env.ORIGIN + "/api/v1/me").catch(() => null)
const status = me ? me.status() : "request-failed"
await browser.close()
if (status !== 401) {
  console.error("signed out but /api/v1/me answered " + status)
  process.exit(1)
}
console.log("signed out")
