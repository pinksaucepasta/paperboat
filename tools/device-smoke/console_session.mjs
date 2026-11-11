// Signs one account in to the console in headless Chromium and saves its session (storage state, mode 600) to OUT.
// Nothing secret is printed; secrets come from files named on the command line.
//   password SSO-less account: node console_session.mjs password EMAIL PASSWORD_FILE OUT
//   SSO user (IdP-initiated, Keycloak form then the WorkOS consent page):
//                              node console_session.mjs sso USER FIXTURE_JSON OUT      env: IDP_START (the IdP's SP-initiated URL)
// env: ORIGIN (console origin), PW_CHROMIUM, [PW_MODULE]. FIXTURE_JSON holds users.<USER> (the Keycloak password).
import { chmodSync, readFileSync } from "node:fs"

const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const [mode, ...rest] = process.argv.slice(2)
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
const context = await browser.newContext()
const page = await context.newPage()
let out
if (mode === "password") {
  const [email, passwordFile] = rest
  out = rest[2]
  await page.goto(process.env.ORIGIN + "/")
  await page.waitForTimeout(3000)
  const link = page.getByRole("link", { name: /sign in/i })
  if (await link.count()) await link.first().click()
  await page.waitForSelector('input[name="email"]', { timeout: 30000 })
  await page.fill('input[name="email"]', email)
  await page.keyboard.press("Enter")
  await page.waitForSelector('input[type="password"]', { timeout: 30000 })
  await page.fill('input[type="password"]', readFileSync(passwordFile, "utf8").trim())
  await page.keyboard.press("Enter")
} else if (mode === "sso") {
  const [user, fixture] = rest
  out = rest[2]
  await page.goto(process.env.IDP_START)
  await page.waitForSelector("#username", { timeout: 60000 })
  await page.fill("#username", user)
  await page.fill("#password", JSON.parse(readFileSync(fixture, "utf8")).users[user])
  await page.click("#kc-login")
  await page.waitForURL(/signin-consent/, { timeout: 60000 }).catch(() => {})
  const consent = page.locator('button[type="submit"], input[type="submit"]')
  if (/signin-consent/.test(page.url()) && (await consent.count())) await consent.first().click()
} else {
  console.error("usage: password EMAIL PASSWORD_FILE OUT | sso USER FIXTURE_JSON OUT")
  process.exit(2)
}
await page.waitForTimeout(8000)
const me = await context.request.get(process.env.ORIGIN + "/api/v1/me")
if (!me.ok()) {
  console.error("console session not established: HTTP " + me.status())
  process.exit(1)
}
await context.storageState({ path: out })
chmodSync(out, 0o600)
console.log("session saved")
await browser.close()
