// The operator admin panel on the live deployment, READ ONLY: signs in as an operator (email, password, authenticator
// code), opens every panel page and one machine, team and user from their lists, and signs out. A page passes when it has
// its heading, no loading placeholder stays, no error is shown and no API call of the page fails. Nothing is changed.
// The credential file (mode 600: email, password, authenticator key on three lines) is read here and never printed.
// usage: ADMIN_ORIGIN=<panel origin> OPERATOR_CRED=<file> [PW_MODULE=...] node console_admin.mjs
import { createHmac } from "node:crypto"
import { readFileSync } from "node:fs"

const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const origin = process.env.ADMIN_ORIGIN.replace(/\/$/, "")
const [email, password, key] = readFileSync(process.env.OPERATOR_CRED, "utf8").split("\n").map((line) => line.trim())
const pages = ["/", "/overview", "/users", "/teams", "/machines", "/billing", "/money", "/traffic", "/analytics", "/capacity", "/fleet", "/infrastructure",
  "/releases", "/security", "/ip-blocks", "/abuse", "/support", "/announcements", "/notices", "/status", "/sentry", "/backups", "/audit", "/operators"]
const details = ["/machines", "/teams", "/users"]
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const redact = (text) => text.replace(/[^\s@"]+@[^\s@"]+/g, "<email>")

function totp(secret, offset = 0) {
  let bits = ""
  for (const ch of secret.replace(/[\s=]/g, "").toUpperCase()) bits += "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567".indexOf(ch).toString(2).padStart(5, "0")
  const counter = Buffer.alloc(8)
  counter.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 30000) + offset))
  const mac = createHmac("sha1", Buffer.from(bits.match(/.{8}/g).map((byte) => parseInt(byte, 2)))).update(counter).digest()
  const at = mac[mac.length - 1] & 15
  return String((mac.readUInt32BE(at) & 0x7fffffff) % 1000000).padStart(6, "0")
}

const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
const context = await browser.newContext({ viewport: { width: 1440, height: 900 } })
const page = await context.newPage()
let broken = []
page.on("response", (r) => {
  const path = new URL(r.url()).pathname
  // "no break-glass run yet" is a 404 the Security page shows as its empty state (components/breakglass.tsx)
  if (path.startsWith("/api/") && r.status() >= 400 && !(r.status() === 404 && path === "/api/v1/admin/breakglass/runs/latest")) broken.push(`${r.request().method()} ${path.replace(/[A-Za-z]+_[A-Za-z0-9-]{8,}/g, ":id")} ${r.status()}`)
})
page.on("pageerror", (e) => broken.push("page error: " + String(e.message).split("\n")[0].slice(0, 120)))

async function visit(path) {
  broken = []
  const response = await page.goto(origin + path, { waitUntil: "domcontentloaded", timeout: 120000 }).catch(() => null)
  await page.waitForLoadState("networkidle", { timeout: 30000 }).catch(() => {})
  let state
  for (let i = 0; i < 40; i++) {
    await page.waitForTimeout(500)
    state = await page.evaluate(() => {
      const main = document.querySelector("main") ?? document.body
      return {
        // a record page has an h1; a list page's title is the current breadcrumb (admin/components/page.tsx)
        heading: ([...document.querySelectorAll("h1, header [aria-current=page]")].find((h) => h.offsetParent)?.textContent ?? "").trim(),
        loading: [...main.querySelectorAll("[data-slot=skeleton]")].filter((e) => e.offsetParent).length,
        alerts: [...main.querySelectorAll("[role=alert], [data-error]")].filter((e) => e.offsetParent).map((e) => e.innerText.replace(/\s+/g, " ").slice(0, 120)),
        text: main.innerText,
      }
    }).catch(() => ({ heading: "", loading: 1, alerts: [], text: "" }))
    if (state.heading && !state.loading) break
  }
  const odd = state.text.match(/\[object Object\]|\bNaN\b|\bundefined\b|couldn[’']t load|something went wrong/i)?.[0]
  const ok = (response?.status() ?? 0) === 200 && Boolean(state.heading) && !state.loading && !state.alerts.length && !broken.length && !odd && !page.url().includes("/login")
  log(ok, `admin ${path.replace(/[A-Za-z]+_[A-Za-z0-9-]{8,}/g, ":id")}`, ok ? state.heading : redact(
    [`HTTP ${response?.status() ?? 0}`, `heading "${state.heading}"`, state.loading ? `${state.loading} placeholders` : "", state.alerts[0] ?? "", odd ? `text "${odd}"` : "", ...broken.slice(0, 3)].filter(Boolean).join("; ")))
}

try {
  await page.goto(origin + "/users", { waitUntil: "domcontentloaded", timeout: 120000 })
  log(new URL(page.url()).pathname === "/login", "a signed-out visitor goes to the sign-in page")
  const anonymous = await context.request.get(origin + "/api/v1/admin/accounts").catch(() => null)
  log([401, 403].includes(anonymous?.status()), "the operator API refuses a signed-out caller", `HTTP ${anonymous?.status()}`)
  await page.fill("#email", email, { timeout: 30000 })
  await page.keyboard.press("Enter")
  await page.locator("#password").waitFor({ timeout: 30000 })
  await page.fill("#password", password)
  await page.keyboard.press("Enter")
  await page.locator("#code").waitFor({ timeout: 30000 })
  log(true, "a password alone is not enough: the second factor is asked for")
  let signedIn = false
  for (const offset of [0, 1, 2]) {
    if (offset === 2) await page.waitForTimeout(31000) // a code works once: another run may have used these
    await page.fill("#code", totp(key, offset === 2 ? 0 : offset))
    await page.keyboard.press("Enter")
    signedIn = await page.waitForURL((u) => u.pathname !== "/login", { timeout: 15000 }).then(() => true, () => false)
    if (signedIn) break
  }
  log(signedIn, "the authenticator code signs the operator in")
  if (signedIn) {
    for (const path of pages) await visit(path)
    for (const list of details) {
      await page.goto(origin + list, { waitUntil: "domcontentloaded", timeout: 120000 })
      const link = page.locator(`main a[href^="${list}/"]`).first()
      const href = await link.waitFor({ timeout: 30000 }).then(() => link.getAttribute("href"), () => null)
      if (href) await visit(href)
      else log(false, `admin ${list}/:id`, "the list links no record")
    }
  }
} catch (error) {
  log(false, "admin check stopped", redact(`${new URL(page.url()).pathname}: ${String(error.message).split("\n")[0].slice(0, 200)}`))
} finally {
  await context.request.post(origin + "/api/v1/operator/logout", { headers: { origin } }).catch(() => {})
  await browser.close()
}
process.exit(failed ? 1 : 0)
