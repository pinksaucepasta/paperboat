// F2-09 after the deletion: the confirmation mail arrives in the account's disposable inbox, and the same address signs up
// again as a new account without any of the old one's machines; that account is deleted by the caller (state.json).
// usage: ORIGIN=... STATE=<DIR/state.json> [PW_MODULE=...] node console_deleted_account.mjs DIR
import { chmodSync, readFileSync, writeFileSync } from "node:fs"
import { mailtm, signUpForm, waitMessage } from "./console_signup.mjs"

const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const origin = process.env.ORIGIN
const dir = process.argv[2]
const account = JSON.parse(readFileSync(`${dir}/account.json`, "utf8"))
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const seen = new Set()
const mail = await waitMessage(account.mailToken, seen, (m) => /delet/i.test(m.subject ?? ""), 300000)
log(Boolean(mail), "the account deletion confirmation email arrives", mail ? mail.subject : "no such message in 5 minutes")
for (const message of (await mailtm("/messages", account.mailToken))["hydra:member"] ?? []) seen.add(message.id)

const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
const context = await browser.newContext()
const page = await context.newPage()
try {
  await page.goto(origin + "/")
  await signUpForm(page, account, seen)
  await page.waitForURL((u) => u.href.startsWith(origin), { waitUntil: "domcontentloaded", timeout: 240000 }).finally(async () => {
    writeFileSync(`${dir}/state.json`, JSON.stringify(await context.storageState()), { mode: 0o600 })
  })
  let me = 0
  for (let i = 0; i < 6 && me !== 200; i++) {
    me = await page.evaluate(async () => (await fetch("/api/v1/me").catch(() => ({ status: 0 }))).status)
    if (me !== 200) await page.waitForTimeout(5000)
  }
  log(me === 200, "the same address signs up again", `/me ${me}`)
  if (me === 200) {
    writeFileSync(`${dir}/state.json`, JSON.stringify(await context.storageState()), { mode: 0o600 })
    chmodSync(`${dir}/state.json`, 0o600)
    const machines = await page.evaluate(async () => (await (await fetch("/api/v1/machines?workspace=personal")).json()).data.items.length)
    log(machines === 0, "the new account has none of the old account's machines", `${machines} machines`)
  }
} catch (error) {
  log(false, "signing up again stopped", `${new URL(page.url()).pathname}: ${String(error.message).split("\n")[0].slice(0, 200)}`)
} finally {
  await browser.close()
}
process.exit(failed ? 1 : 0)
