// Headless check for F2-09 account deletion on the live console. It DELETES the account, so run
// it only with a throwaway: STATE=<its Playwright storage state> EMAIL=<its email> CHROME=<a
// Chromium> CONSOLE_ORIGIN=<console origin> node check-account-deletion.mjs (playwright-core
// installed). Checks: Account offers Delete account…; the page lists what goes; the red button
// stays disabled until the exact email is typed; DELETE answers 204; the page ends on
// /auth/deleted; the session is gone (/me 401). Confirm afterwards: the machine's `pb status` says
// signed out, the deletion email arrived, and the email can sign up again as a new account.
import { chromium } from "playwright-core"

const { STATE, EMAIL, CHROME, CONSOLE_ORIGIN: origin } = process.env
const steps = []
const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })
const browser = await chromium.launch({ executablePath: CHROME })
const page = await (await browser.newContext({ storageState: STATE })).newPage()
await page.goto(`${origin}/account`)
await page.waitForTimeout(5000)
pass("Account offers Delete account…", (await page.getByText("Delete account…").count()) === 1)
await page.getByText("Delete account…").click()
await page.waitForURL(/\/account\/delete/)
await page.waitForTimeout(5000)
const text = (await page.locator("main main >> visible=true").first().innerText()).replace(/\s+/g, " ")
pass("the page lists what is deleted", /What is deleted/.test(text) && /Machines/.test(text) && /Subscription/.test(text))
const button = page.getByRole("button", { name: "Delete my account" })
pass("the button is disabled until the email is typed", await button.isDisabled())
await page.fill("#confirm-email", "someone-else@example.test")
pass("a different email keeps it disabled", await button.isDisabled())
await page.fill("#confirm-email", EMAIL)
pass("the exact email enables it", !(await button.isDisabled()))
let status = 0
page.on("response", (r) => {
  if (new URL(r.url()).pathname === "/api/v1/account" && r.request().method() === "DELETE") status = r.status()
})
await button.click()
await page.waitForURL(/\/auth\/deleted/, { timeout: 60000 }).catch(() => {})
pass("DELETE /v1/account answers 204", status === 204, String(status))
pass("the page ends on /auth/deleted", new URL(page.url()).pathname === "/auth/deleted")
pass("the session is gone", (await page.evaluate(async () => (await fetch("/api/v1/me")).status)) === 401)
for (const s of steps) console.log(`${s.ok ? "PASS" : "FAIL"}  ${s.step}${s.detail ? `  (${s.detail})` : ""}`)
await browser.close()
process.exit(steps.every((s) => s.ok) ? 0 : 1)
