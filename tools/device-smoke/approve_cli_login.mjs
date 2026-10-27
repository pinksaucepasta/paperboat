// Approves a pb login link in headless Chromium signed in with a stored console session (STATE, mode 600, never printed).
// usage: STATE=<storage state> PW_CHROMIUM=<chromium> [PW_MODULE=<playwright-core index.mjs>] node approve_cli_login.mjs <approval link>
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
const page = await (await browser.newContext({ storageState: process.env.STATE })).newPage()
await page.goto(process.argv[2])
await page.getByRole("button", { name: /^(Sign in|Add)$/ }).click({ timeout: 60000 })
await page.waitForFunction(() => !document.querySelector("button[disabled]"), null, { timeout: 30000 }).catch(() => {})
await page.waitForTimeout(3000)
console.log("approved")
await browser.close()
