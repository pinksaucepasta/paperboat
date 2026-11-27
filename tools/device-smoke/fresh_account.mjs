// A throwaway console account for the final re-check: signup creates it (disposable mail.tm inbox, AuthKit sign-up) and
// leaves its session in DIR/state.json; delete removes it again through the console's Delete account page.
// usage: ORIGIN=<console origin> PW_CHROMIUM=<chromium> [PW_MODULE=<playwright-core index.mjs>] node fresh_account.mjs signup|delete|reauth DIR
// DIR is created mode 700, its files mode 600; nothing secret is printed.
import { chmodSync, mkdirSync, readFileSync, writeFileSync } from "node:fs"
import { randomBytes } from "node:crypto"
import { mailtm, signUpForm } from "./console_signup.mjs"

const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const origin = process.env.ORIGIN
const [action, dir] = process.argv.slice(2)
const accountFile = `${dir}/account.json`
const stateFile = `${dir}/state.json`
const write = (file, text) => (writeFileSync(file, text, { mode: 0o600 }), chmodSync(file, 0o600))
// The live console is a dev server that compiles a route on its first request: ask for each route this script uses until it answers
// (anything below 500, redirects not followed), for at most five minutes in all.
async function warm() {
  const deadline = Date.now() + 300000
  for (const path of ["/", "/auth/sign-in", "/auth/callback", "/machines", "/api/v1/me", "/account/delete"]) {
    while (Date.now() < deadline) {
      const status = await fetch(origin + path, { redirect: "manual", signal: AbortSignal.timeout(60000) }).then((r) => r.status, () => 0)
      if (status > 0 && status < 500) break
      await new Promise((r) => setTimeout(r, 5000))
    }
  }
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
  await signUpForm(page, { email, password, mailToken })
  await page.waitForURL((u) => u.href.startsWith(origin), { waitUntil: "domcontentloaded", timeout: 420000 }) // the live console compiles the callback route on first use (minutes on a busy dev server)
  write(stateFile, JSON.stringify(await context.storageState()))
  let me = 0
  for (let i = 0; i < 6 && me !== 200; i++) { // the live console's dev server drops a connection now and then
    me = await page.evaluate(async () => (await fetch("/api/v1/me").catch(() => ({ status: 0 }))).status)
    if (me !== 200) await page.waitForTimeout(5000)
  }
  await browser.close()
  if (me !== 200) throw new Error(`signed up but /me answers ${me}`)
  console.log("fresh account signed up")
}

// The server asks for a sign-in from the last five minutes before it deletes an account, and a run takes longer: sign in again
// (email, password, and the emailed code from the disposable inbox) and come back to the delete page.
async function reauthenticate(page, { email, password, mailToken }) {
  const seen = new Set(((await mailtm("/messages", mailToken))["hydra:member"] ?? []).map((m) => m.id))
  // The delete page may already have sent the browser to the sign-in page.
  const again = page.getByRole("link", { name: "Sign in again" }).or(page.getByRole("button", { name: "Sign in again" })).first()
  if (page.url().startsWith(origin) && (await again.waitFor({ timeout: 30000 }).then(() => true, () => false))) await again.click()
  // Then whatever the sign-in pages ask, in the order they ask it, until the console is back. A page loads when it
  // loads: each field is filled once, when it shows.
  const code = 'input[inputmode="numeric"]:visible, input[name="code"]:visible, input[autocomplete="one-time-code"]:visible'
  const done = new Set()
  const deadline = Date.now() + 420000 // the live console compiles the callback route on first use (minutes on a busy dev server)
  while (Date.now() < deadline) {
    await page.waitForTimeout(3000)
    const url = new URL(page.url())
    if (url.origin === origin && !url.pathname.startsWith("/auth/")) {
      if (done.size || (await page.locator("#confirm-email").count().catch(() => 0))) return
      if (await again.isVisible().catch(() => false)) await again.click().catch(() => {})
      continue
    }
    // Only a field that is still on the same page a moment later takes text: a page on its way out loses what is typed.
    const shown = async (selector) => {
      const before = page.url()
      if (!(await page.locator(selector).count().catch(() => 0))) return false
      await page.waitForTimeout(7000) // the sign-in form collects its browser signals for a few seconds first
      return page.url() === before && (await page.locator(selector).count().catch(() => 0)) > 0
    }
    // A password typed while the form was still loading is refused as wrong: it is typed once more.
    if (done.has("password") && !done.has("password-again") && (await page.getByText("Invalid email or password").count().catch(() => 0)) && (await shown('input[type="password"]:visible'))) {
      await page.fill('input[type="password"]:visible', password)
      await page.keyboard.press("Enter")
      done.add("password-again")
      continue
    }
    if (!done.has("code") && (await shown(code))) {
      let sent = ""
      for (let i = 0; i < 30 && !sent; i++) {
        const fresh = ((await mailtm("/messages", mailToken))["hydra:member"] ?? []).find((m) => !seen.has(m.id))
        if (fresh) sent = ((await mailtm(`/messages/${fresh.id}`, mailToken)).text ?? "").match(/\b(\d{6})\b/)?.[1] ?? ""
        else await new Promise((r) => setTimeout(r, 4000))
      }
      if (!sent) throw new Error("no sign-in code arrived in the disposable inbox")
      const boxes = page.locator(code)
      if ((await boxes.count()) > 1) (await boxes.first().click(), await page.keyboard.type(sent))
      else await boxes.first().fill(sent)
      await page.keyboard.press("Enter").catch(() => {})
      done.add("code")
    } else if (!done.has("password") && (await shown('input[type="password"]:visible'))) {
      await page.fill('input[type="password"]:visible', password)
      await page.keyboard.press("Enter")
      done.add("password")
    } else if (!done.has("email") && !done.has("password") && (await shown('input[name="email"]:not([readonly]):visible'))) {
      // A re-authentication page shows the account's email read-only or hidden.
      await page.fill('input[name="email"]:not([readonly]):visible', email)
      await page.keyboard.press("Enter")
      done.add("email")
    }
  }
  const text = await page.locator("body").innerText().catch(() => "")
  // which account the form names, never the values themselves
  const form = await page.evaluate((expected) => [...document.querySelectorAll("input")].filter((i) => i.offsetParent !== null)
    .map((i) => `${i.name}:${i.value.length}${i.name === "email" && i.value ? (i.value === expected ? " (this account)" : " (ANOTHER address)") : ""}`).join(" "), email).catch(() => "")
  console.error(`sign-in form inputs: ${form}`)
  throw new Error(`signing in again did not come back to the console: at ${new URL(page.url()).host}${new URL(page.url()).pathname} after ${[...done].join(",") || "no step"}: ${text.replace(/[^\s@]+@[^\s@]+/g, "<email>").replace(/\s+/g, " ").slice(0, 200)}`)
}

async function remove() {
  const account = JSON.parse(readFileSync(accountFile, "utf8"))
  const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
  const page = await (await browser.newContext({ storageState: stateFile })).newPage()
  for (let i = 0; i < 4; i++) { // same dropped connections as in signup
    if (await page.goto(`${origin}/account/delete`).then(() => true, () => false)) break
    await page.waitForTimeout(10000)
  }
  await page.waitForTimeout(5000)
  if (!(await page.locator("#confirm-email").count())) {
    await reauthenticate(page, account)
    await page.goto(`${origin}/account/delete`)
    await page.waitForTimeout(5000)
  }
  // the page asks for the address the account is stored under, which is not the typed one for a plus-address sign-up
  const stored = await page.request.get(origin + "/api/v1/me").then(async (r) => (r.ok() ? (await r.json()).data?.email : undefined), () => undefined)
  let status = 0
  page.on("response", (r) => {
    if (new URL(r.url()).pathname === "/api/v1/account" && r.request().method() === "DELETE") status = r.status()
  })
  for (let attempt = 0; attempt < 2 && status !== 204; attempt++) {
    if (attempt) { // the delete page looked fresh but the server found the last sign-in too old (401): sign in again once
      await reauthenticate(page, account)
      await page.goto(`${origin}/account/delete`)
      await page.waitForTimeout(5000)
    }
    status = 0
    await page.fill("#confirm-email", stored ?? account.email, { timeout: 15000 })
    await page.getByRole("button", { name: "Delete my account" }).click()
    await page.waitForURL(/\/auth\/deleted/, { timeout: 60000 }).catch(() => {})
  }
  await browser.close()
  if (status !== 204) throw new Error(`account deletion answered ${status}`)
  console.log("fresh account deleted")
}

// A new sign-in for the stored session, for what asks for one from the last five minutes; the new session is saved.
async function reauth() {
  const account = JSON.parse(readFileSync(accountFile, "utf8"))
  const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
  try {
  const context = await browser.newContext({ storageState: stateFile })
  const page = await context.newPage()
  await page.goto(`${origin}/account/delete`)
  await page.waitForTimeout(5000)
  if (!(await page.locator("#confirm-email").count())) await reauthenticate(page, account)
  write(stateFile, JSON.stringify(await context.storageState()))
  console.log("signed in again")
  } finally {
    await browser.close()
  }
}

try {
  if (action === "signup" || action === "delete" || action === "reauth") await warm()
  if (action === "signup") {
    // One retry on a transport error only (the dev server dropped the connection); the first attempt's mailbox is abandoned.
    await signup().catch(async (e) => {
      if (!/Failed to fetch|Timeout|ERR_CONNECTION|ERR_EMPTY|ECONNRESET/.test(String(e.message))) throw e
      console.error("sign-up hit a transport error, trying once more: " + String(e.message).split("\n")[0])
      await warm()
      await signup()
    })
  } else await (action === "delete" ? remove() : action === "reauth" ? reauth() : Promise.reject(new Error("usage: signup|delete|reauth DIR")))
} catch (e) {
  console.error(String(e.message ?? e))
  process.exit(1)
}
