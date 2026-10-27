// A throwaway console account for the final re-check: signup creates it (disposable mail.tm inbox, AuthKit sign-up) and
// leaves its session in DIR/state.json; delete removes it again through the console's Delete account page.
// usage: ORIGIN=<console origin> PW_CHROMIUM=<chromium> [PW_MODULE=<playwright-core index.mjs>] node fresh_account.mjs signup|delete DIR
// DIR is created mode 700, its files mode 600; nothing secret is printed.
import { chmodSync, mkdirSync, readFileSync, writeFileSync } from "node:fs"
import { randomBytes } from "node:crypto"

const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const origin = process.env.ORIGIN
const [action, dir] = process.argv.slice(2)
const accountFile = `${dir}/account.json`
const stateFile = `${dir}/state.json`
const write = (file, text) => (writeFileSync(file, text, { mode: 0o600 }), chmodSync(file, 0o600))
const mailtm = async (path, token, body) => {
  const r = await fetch("https://api.mail.tm" + path, {
    method: body ? "POST" : "GET",
    headers: { "content-type": "application/json", ...(token ? { authorization: `Bearer ${token}` } : {}) },
    body: body && JSON.stringify(body),
  })
  if (!r.ok) throw new Error(`mail.tm ${path}: HTTP ${r.status}`)
  return r.json()
}

async function signup() {
  mkdirSync(dir, { recursive: true, mode: 0o700 })
  const domain = (await mailtm("/domains")) ["hydra:member"][0].domain
  const email = `final-${randomBytes(5).toString("hex")}@${domain}`
  const password = randomBytes(12).toString("hex") + "Aa1!"
  await mailtm("/accounts", null, { address: email, password })
  const mailToken = (await mailtm("/token", null, { address: email, password })).token
  write(accountFile, JSON.stringify({ email, password, mailToken }))
  const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
  const context = await browser.newContext()
  const page = await context.newPage()
  await page.goto(origin + "/")
  await page.getByRole("link", { name: "Sign up" }).click()
  await page.waitForTimeout(3000)
  for (const [selector, value] of [['input[name="email"]', email], ['input[name="first_name"]', "Final"], ['input[name="last_name"]', "Recheck"]]) {
    if (await page.locator(selector).count()) await page.fill(selector, value)
  }
  await page.keyboard.press("Enter")
  await page.waitForTimeout(4000)
  if (await page.locator('input[type="password"]').count()) {
    await page.fill('input[type="password"]', password)
    await page.keyboard.press("Enter")
    await page.waitForTimeout(5000)
  }
  let code = ""
  for (let i = 0; i < 30 && !code; i++) {
    const [first] = (await mailtm("/messages", mailToken))["hydra:member"]
    if (first) code = ((await mailtm(`/messages/${first.id}`, mailToken)).text ?? "").match(/\b(\d{6})\b/)?.[1] ?? ""
    else await new Promise((r) => setTimeout(r, 4000))
  }
  if (!code) throw new Error("no verification code arrived in the disposable inbox")
  const boxes = page.locator('input[inputmode="numeric"], input[name="code"], input[autocomplete="one-time-code"]')
  if ((await boxes.count()) > 1) (await boxes.first().click(), await page.keyboard.type(code))
  else await boxes.first().fill(code)
  await page.keyboard.press("Enter").catch(() => {})
  await page.waitForURL((u) => u.href.startsWith(origin), { timeout: 60000 })
  write(stateFile, JSON.stringify(await context.storageState()))
  const me = await page.evaluate(async () => (await fetch("/api/v1/me")).status)
  await browser.close()
  if (me !== 200) throw new Error(`signed up but /me answers ${me}`)
  console.log("fresh account signed up")
}

// The server asks for a sign-in from the last five minutes before it deletes an account, and a run takes longer: sign in again
// (email, password, and the emailed code from the disposable inbox) and come back to the delete page.
async function reauthenticate(page, { email, password, mailToken }) {
  const seen = new Set(((await mailtm("/messages", mailToken))["hydra:member"] ?? []).map((m) => m.id))
  // The delete page may already have sent the browser to the sign-in page.
  if (page.url().startsWith(origin)) {
    await page.getByRole("link", { name: "Sign in again" }).or(page.getByRole("button", { name: "Sign in again" })).first().click()
    await page.waitForTimeout(3000)
  }
  // A re-authentication page shows the account's email read-only or hidden.
  if (await page.locator('input[name="email"]:not([readonly]):visible').count()) {
    await page.fill('input[name="email"]:not([readonly]):visible', email)
    await page.keyboard.press("Enter")
    await page.waitForTimeout(3000)
  }
  if (await page.locator('input[type="password"]').count()) {
    await page.fill('input[type="password"]', password)
    await page.keyboard.press("Enter")
    await page.waitForTimeout(4000)
  }
  const boxes = page.locator('input[inputmode="numeric"], input[name="code"], input[autocomplete="one-time-code"]')
  if (await boxes.count()) {
    let code = ""
    for (let i = 0; i < 30 && !code; i++) {
      const fresh = ((await mailtm("/messages", mailToken))["hydra:member"] ?? []).find((m) => !seen.has(m.id))
      if (fresh) code = ((await mailtm(`/messages/${fresh.id}`, mailToken)).text ?? "").match(/\b(\d{6})\b/)?.[1] ?? ""
      else await new Promise((r) => setTimeout(r, 4000))
    }
    if (!code) throw new Error("no sign-in code arrived in the disposable inbox")
    if ((await boxes.count()) > 1) (await boxes.first().click(), await page.keyboard.type(code))
    else await boxes.first().fill(code)
    await page.keyboard.press("Enter").catch(() => {})
  }
  await page.waitForURL((u) => u.href.startsWith(origin), { timeout: 60000 })
}

async function remove() {
  const account = JSON.parse(readFileSync(accountFile, "utf8"))
  const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
  const page = await (await browser.newContext({ storageState: stateFile })).newPage()
  await page.goto(`${origin}/account/delete`)
  await page.waitForTimeout(5000)
  if (!(await page.locator("#confirm-email").count())) {
    await reauthenticate(page, account)
    await page.goto(`${origin}/account/delete`)
    await page.waitForTimeout(5000)
  }
  await page.fill("#confirm-email", account.email, { timeout: 15000 })
  let status = 0
  page.on("response", (r) => {
    if (new URL(r.url()).pathname === "/api/v1/account" && r.request().method() === "DELETE") status = r.status()
  })
  await page.getByRole("button", { name: "Delete my account" }).click()
  await page.waitForURL(/\/auth\/deleted/, { timeout: 60000 }).catch(() => {})
  await browser.close()
  if (status !== 204) throw new Error(`account deletion answered ${status}`)
  console.log("fresh account deleted")
}

try {
  await (action === "signup" ? signup() : action === "delete" ? remove() : Promise.reject(new Error("usage: signup|delete DIR")))
} catch (e) {
  console.error(String(e.message ?? e))
  process.exit(1)
}
