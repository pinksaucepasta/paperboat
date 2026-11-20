// Approves a pb login link in headless Chromium signed in with a stored console session (STATE, mode 600, never printed).
// usage: STATE=<storage state> PW_CHROMIUM=<chromium> [PW_MODULE=<playwright-core index.mjs>] node approve_cli_login.mjs <approval link>
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
const page = await (await browser.newContext({ storageState: process.env.STATE })).newPage()
const response = await page.goto(process.argv[2])
try {
  await page.getByRole("button", { name: /^(Sign in|Add)$/ }).click({ timeout: 60000 })
} catch {
  // One short line (the canary keeps the last 200 characters of stderr): where the page ended up and what it showed, with no query, email or token-like string.
  const url = new URL(page.url())
  const text = (await page.locator("body").innerText({ timeout: 2000 }).catch(() => "(no text)"))
    .replace(/\s+/g, " ")
    .replace(/[^\s@]+@[^\s@]+/g, "<email>")
    .replace(/[A-Za-z0-9_-]{24,}/g, "<token>")
    .slice(0, 90)
  console.error(`no Sign in/Add button: ${url.origin}${url.pathname} status=${response?.status() ?? "none"} text="${text}"`)
  await browser.close()
  process.exit(1)
}
await page.waitForFunction(() => !document.querySelector("button[disabled]"), null, { timeout: 30000 }).catch(() => {})
await page.waitForTimeout(3000)
console.log("approved")
await browser.close()
