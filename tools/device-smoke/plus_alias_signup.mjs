// F13-07 (first sign-up wins, user decision 2026-11-27): a console sign-up with any other spelling of an existing account's
// canonical address (x+tag after x, x after x+tag) is refused, naming the existing account's exact address.
// usage: ORIGIN=<console> PW_CHROMIUM=<chromium> [PW_MODULE=...] node plus_alias_signup.mjs ACCOUNT_DIR [WORK_DIR]
//   ACCOUNT_DIR  a throwaway account made by fresh_account.mjs signup (account.json): its address is the canonical one
//   WORK_DIR     when given, the other half too: a plus address whose canonical has NO account is not refused. It signs that address
//                up on a new disposable inbox and writes WORK_DIR/account.json (fresh_account.mjs delete WORK_DIR removes the account);
//                then the base address signs up as a second login, which must not land in that account ("no silent merge").
// Prints one JSON line {"steps":[...]} and exits 1 when a step failed. No password or code is printed.
import { chmodSync, mkdirSync, readFileSync, writeFileSync } from "node:fs"
import { randomBytes } from "node:crypto"

const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const origin = process.env.ORIGIN
const [accountDir, workDir] = process.argv.slice(2)
const { email: canonical } = JSON.parse(readFileSync(`${accountDir}/account.json`, "utf8"))
const refusal = (address) => `An account with ${address} already exists. Sign in with that address.`
const steps = []
const add = (step, ok, detail = "") => steps.push({ step, ok: Boolean(ok), detail: String(detail).slice(0, 220) })
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })

const mailtm = async (path, token, body) => {
  for (let attempt = 0; ; attempt++) {
    const r = await fetch("https://api.mail.tm" + path, {
      method: body ? "POST" : "GET",
      headers: { "content-type": "application/json", ...(token ? { authorization: `Bearer ${token}` } : {}) },
      body: body && JSON.stringify(body),
    })
    if (r.status === 429 && attempt < 8) {
      await new Promise((resolve) => setTimeout(resolve, Math.min(30, Number(r.headers.get("retry-after")) || 5) * 1000))
      continue
    }
    if (!r.ok) throw new Error(`mail.tm: HTTP ${r.status}`)
    return r.json()
  }
}

// The console's "Sign up" up to AuthKit's answer for the address: "refused", "code" (the email verification step) or "console" (signed in).
async function signUp(address, password) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 1000 } })
  const page = await context.newPage()
  for (let attempt = 0; ; attempt++) {
    // the live console compiles a route on its first request and drops a connection now and then
    const loaded = await page.goto(origin + "/", { waitUntil: "domcontentloaded", timeout: 180000 }).then(() => true, () => false)
    if (loaded && (await page.getByRole("link", { name: "Sign up" }).waitFor({ timeout: 60000 }).then(() => true, () => false))) break
    if (attempt === 3) throw new Error("the console's Sign up link did not appear")
    await page.waitForTimeout(8000)
  }
  await page.getByRole("link", { name: "Sign up" }).click()
  // the sign-in page has an email field too: only the sign-up page asks for the name
  await page.locator('input[name="first_name"]').waitFor({ timeout: 180000 })
  for (const [name, value] of [["email", address], ["first_name", "Final"], ["last_name", "Recheck"]]) {
    if (await page.locator(`input[name="${name}"]`).count()) await page.fill(`input[name="${name}"]`, value)
  }
  await page.keyboard.press("Enter")
  const codeBoxes = page.locator('input[inputmode="numeric"], input[name="code"], input[autocomplete="one-time-code"]')
  let filledPassword = false
  for (let i = 0; i < 60; i++) {
    await page.waitForTimeout(1000)
    const text = (await page.locator("body").innerText().catch(() => "")).replace(/\s+/g, " ")
    if (/already exists/i.test(text)) return { outcome: "refused", text, page, context }
    if (page.url().startsWith(origin) && !/\/auth\//.test(page.url()) && (await context.request.get(origin + "/api/v1/me")).ok()) return { outcome: "console", text, page, context }
    if (await codeBoxes.count()) return { outcome: "code", text, page, context, codeBoxes, password }
    if (!filledPassword && (await page.locator('input[type="password"]:visible').count())) {
      await page.fill('input[type="password"]:visible', password)
      await page.keyboard.press("Enter")
      filledPassword = true
    }
  }
  return { outcome: "stuck", text: (await page.locator("body").innerText().catch(() => "")).replace(/\s+/g, " "), page, context }
}

// Types the newest emailed code that is not in `seen` into the verification step and waits for the console.
async function confirmCode({ page, codeBoxes }, mailToken, seen) {
  let code = ""
  for (let i = 0; i < 30 && !code; i++) {
    const fresh = (await mailtm("/messages", mailToken))["hydra:member"].find((m) => !seen.has(m.id))
    if (fresh) code = ((await mailtm(`/messages/${fresh.id}`, mailToken)).text ?? "").match(/\b(\d{6})\b/)?.[1] ?? ""
    else await new Promise((r) => setTimeout(r, 4000))
  }
  if (!code) throw new Error("no verification code arrived in the disposable inbox")
  if ((await codeBoxes.count()) > 1) (await codeBoxes.first().click(), await page.keyboard.type(code))
  else await codeBoxes.first().fill(code)
  await page.keyboard.press("Enter").catch(() => {})
  await page.waitForURL((u) => u.href.startsWith(origin), { waitUntil: "domcontentloaded", timeout: 420000 })
}

try {
  const [local, domain] = canonical.split("@")
  // a second +tag and capitals: the address is lowercased and cut at the FIRST +
  for (const [name, alias] of [["plus address", `${local}+smoke@${domain}`], ["capitals and two tags", `${local.toUpperCase()}+Smoke+two@${domain}`]]) {
    const result = await signUp(alias, randomBytes(12).toString("hex") + "Aa1!")
    add(`${name} of an existing account: sign-up is refused`, result.outcome === "refused", `outcome=${result.outcome}`)
    add(`${name}: the refusal names the existing address`, result.text.includes(refusal(canonical)), result.text.includes(refusal(canonical)) ? "" : result.text.slice(0, 200))
    const me = await result.context.request.get(origin + "/api/v1/me")
    add(`${name}: no console session came of it`, !me.ok(), `/v1/me ${me.status()}`)
    await result.context.close()
  }
  if (workDir) {
    mkdirSync(workDir, { recursive: true, mode: 0o700 })
    const mailDomain = (await mailtm("/domains"))["hydra:member"][0].domain
    const base = `final-${randomBytes(5).toString("hex")}@${mailDomain}`
    const alias = base.replace("@", "+solo@")
    const password = randomBytes(12).toString("hex") + "Aa1!"
    await mailtm("/accounts", null, { address: base, password })
    const mailToken = (await mailtm("/token", null, { address: base, password })).token
    const file = `${workDir}/account.json`
    writeFileSync(file, JSON.stringify({ email: alias, password, mailToken }), { mode: 0o600 })
    chmodSync(file, 0o600)
    const result = await signUp(alias, password)
    add("a plus address with no account for its base address is not refused", ["code", "console"].includes(result.outcome), `outcome=${result.outcome} ${result.outcome === "stuck" ? result.text.slice(0, 150) : ""}`)
    if (result.outcome === "code") await confirmCode(result, mailToken, new Set())
    let me = 0
    for (let i = 0; i < 6 && me !== 200; i++) {
      me = (await result.context.request.get(origin + "/api/v1/me").catch(() => ({ status: () => 0 }))).status()
      if (me !== 200) await result.page.waitForTimeout(5000)
    }
    // signed in, the account can be deleted again; an unverified login could not, so that is a failure to report
    add("that sign-up completes into a console account", me === 200, `/v1/me ${me}`)
    if (me === 200) {
      const state = `${workDir}/state.json`
      writeFileSync(state, JSON.stringify(await result.context.storageState()), { mode: 0o600 })
      chmodSync(state, 0o600)
      // The other order: the base address signs up afterwards. First sign-up wins, so it is refused naming the plus address.
      const idOf = async (context) => {
        const reply = await context.request.get(origin + "/api/v1/me")
        const body = reply.ok() ? await reply.json() : {}
        return (body.data ?? body).id
      }
      const plusAccount = await idOf(result.context)
      const seen = new Set((await mailtm("/messages", mailToken))["hydra:member"].map((m) => m.id))
      const laterPassword = randomBytes(12).toString("hex") + "Aa1!"
      const later = await signUp(base, laterPassword)
      let unfinished = ""
      if (later.outcome === "code") await confirmCode(later, mailToken, seen).catch((error) => (unfinished = String(error.message).split("\n")[0]))
      let laterAccount
      for (let i = 0; i < 12 && !laterAccount && later.outcome !== "refused"; i++) {
        laterAccount = await idOf(later.context).catch(() => undefined)
        if (!laterAccount) await later.page.waitForTimeout(5000)
      }
      // a sign-up that neither finished nor was refused proves nothing: that is a failure, never a pass
      // first sign-up wins: the base address is refused, naming the plus address that signed up first
      add("the base address signing up after the plus address is refused, naming the plus address", Boolean(plusAccount) && later.outcome === "refused" && later.text.includes(refusal(alias)),
        `outcome=${later.outcome} second login ${laterAccount ? (laterAccount === plusAccount ? "same account" : "a separate account") : "not signed in"} ${unfinished}`)
      if (laterAccount && laterAccount !== plusAccount) {
        // it made an account of its own: that one is deleted by the caller too (a sibling directory, so deleting one keeps the other's login)
        mkdirSync(`${workDir}-later`, { recursive: true, mode: 0o700 })
        writeFileSync(`${workDir}-later/account.json`, JSON.stringify({ email: base, password: laterPassword, mailToken }), { mode: 0o600 })
        writeFileSync(`${workDir}-later/state.json`, JSON.stringify(await later.context.storageState()), { mode: 0o600 })
      }
      await later.context.close()
    }
  }
} catch (error) {
  add("plus-alias sign-up check completed", false, String(error?.message ?? error).split("\n")[0])
} finally {
  await browser.close()
}
console.log(JSON.stringify({ steps }))
process.exit(steps.length && steps.every((s) => s.ok) ? 0 : 1)
