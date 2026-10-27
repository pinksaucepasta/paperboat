// Headless check for account deletion when the sign-in is more than five minutes old (F2-09): clicking Delete
// account… goes straight to sign-in (no "Sign in again" stop), and after signing in the typed confirmation is back
// with the email field focused. It signs in with a real password and email code but does NOT delete the account.
// STATE=<storage state signed in more than five minutes ago> EMAIL=<its email> PASSWORD=<its password>
// MAIL_CODE_CMD=<command printing the newest emailed sign-in code> CHROME=<a Chromium> CONSOLE_ORIGIN=<console origin>
// node check-account-deletion-reauth.mjs (playwright-core installed).
import { chromium } from "playwright-core"
import { execSync } from "node:child_process"

const { STATE, EMAIL, PASSWORD, MAIL_CODE_CMD, CHROME, CONSOLE_ORIGIN: origin } = process.env
const steps = []
const pass = (step, ok, detail = "") => steps.push({ step, ok, detail })
const browser = await chromium.launch({ executablePath: CHROME })
const page = await (await browser.newContext({ storageState: STATE })).newPage()
const stop = async () => {
  for (const s of steps) console.log(`${s.ok ? "PASS" : "FAIL"}  ${s.step}${s.detail ? `  (${s.detail})` : ""}`)
  await browser.close()
  process.exit(steps.every((s) => s.ok) ? 0 : 1)
}
let stopShown = false
page.on("framenavigated", async () => {
  if (new URL(page.url()).origin === origin) stopShown ||= (await page.getByText("Sign in again", { exact: true }).count().catch(() => 0)) > 0
})
await page.goto(`${origin}/account`)
await page.getByText("Delete account…").click()
await page.waitForURL((url) => url.origin !== origin, { timeout: 30000 }).catch(() => {})
pass("Delete account… goes straight to sign-in", new URL(page.url()).origin !== origin, new URL(page.url()).host)
pass("no 'Sign in again' stop on the way", !stopShown)
if (new URL(page.url()).origin === origin) await stop()
// WorkOS re-authentication ("Confirm it's you") asks for the password only, with the email filled in and locked.
const email = page.getByLabel("Email")
if (await email.isEditable().catch(() => false)) {
  await email.fill(EMAIL)
  await page.getByRole("button", { name: "Continue" }).click()
}
const password = page.locator("input[name=password]")
await password.waitFor({ timeout: 20000 }).catch(() => {})
if (await password.count()) {
  await password.fill(PASSWORD)
  await page.getByRole("button", { name: /Continue|Sign in|Confirm/ }).click()
}
await page.waitForTimeout(4000)
if (/code/i.test((await page.innerText("body")).slice(0, 600))) {
  await page.keyboard.type(execSync(MAIL_CODE_CMD).toString().trim(), { delay: 80 })
}
await page.waitForURL((url) => url.origin === origin, { timeout: 60000 }).catch(() => {})
await page.waitForSelector("#confirm-email", { timeout: 30000 }).catch(() => {})
pass("signing in lands on the deletion confirmation", new URL(page.url()).pathname === "/account/delete", new URL(page.url()).pathname)
pass("the email field is ready to type in", await page.evaluate(() => document.activeElement?.id === "confirm-email"))
pass("the red button waits for the email", await page.getByRole("button", { name: "Delete my account" }).isDisabled().catch(() => false))
await stop()
