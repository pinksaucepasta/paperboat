// Headless check on a live console, ONE throwaway account without an ENV vault (tools/device-smoke/fresh_account.mjs signup DIR):
//   ORIGIN=<console origin> STATE=<storage state> PW_CHROMIUM=<chromium> [PW_MODULE=<playwright-core index.mjs>] node check-env-form-and-rotate-error.mjs
// 1. The setup form keeps what is typed while the vault answer arrives late (CPU slowed) and every later refetch (tab focus)
//    fails with 429: the form is never replaced by the error line and never reset.
// 2. A personal key rotation the server refuses keeps its dialog open with the error named; the vault is locked behind it
//    and the same password unlocks it again. Passwords are made here and never printed.
import { randomBytes } from "node:crypto"

const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const { ORIGIN: origin, STATE: state, PW_CHROMIUM: executablePath } = process.env
const long = { timeout: Number(process.env.LONG_MS ?? 90000) }
const steps = []
const pass = (step, ok, detail = "") => {
  steps.push({ step, ok })
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
async function step(name, run) {
  const started = Date.now()
  try {
    const detail = await run()
    pass(name, true, `${Math.round((Date.now() - started) / 1000)} s${typeof detail === "string" && detail ? `, ${detail}` : ""}`)
  } catch (error) {
    pass(name, false, String(error.message ?? error).split("\n")[0].slice(0, 160))
    throw error
  }
}
const secret = `pw-${randomBytes(9).toString("hex")}`
const browser = await chromium.launch({ executablePath })
const context = await browser.newContext({ storageState: state, viewport: { width: 1280, height: 1000 } })
const page = await context.newPage()
const button = (name) => page.getByRole("button", { name, exact: true })
const notFound = { status: 404, json: { error: { code: "not_found", message: "none" } } }

try {
  await step("the setup form keeps typed text while the vault answer is late and every refetch fails (429)", async () => {
    let answers = 0
    await page.route("**/api/v1/environment-vault*", async (route) => {
      if (route.request().method() !== "GET") return route.continue()
      answers++
      if (answers === 1) {
        await new Promise((resolve) => setTimeout(resolve, 3000))
        return route.fulfill(notFound)
      }
      return route.fulfill({ status: 429, json: { error: { code: "rate_limited", message: "stub: too many requests" } } })
    })
    const cdp = await context.newCDPSession(page)
    await cdp.send("Emulation.setCPUThrottlingRate", { rate: 4 })
    await page.goto(`${origin}/environment`)
    await page.getByText("Create your vault", { exact: true }).waitFor(long)
    const password = page.locator("#vault-new-password")
    const confirm = page.locator("#vault-confirm-password")
    await password.fill(secret)
    await confirm.fill(secret)
    // Tab focus makes the stale vault query refetch; each one is refused. Typed once, then waited out.
    for (let i = 0; i < 4; i++) {
      await page.evaluate(() => document.dispatchEvent(new Event("visibilitychange", { bubbles: true })))
      await page.waitForTimeout(1500)
    }
    if (answers < 3) throw new Error(`only ${answers} vault answers: the refetches did not happen`)
    if ((await password.inputValue()) !== secret || (await confirm.inputValue()) !== secret) throw new Error("the typed text was cleared by the page")
    // (The Vault settings row below may name the refused refetch; only the form itself must stay.)
    if (!(await page.getByText("Create your vault", { exact: true }).isVisible())) throw new Error("the refetch error replaced the form")
    await cdp.send("Emulation.setCPUThrottlingRate", { rate: 1 })
    await page.unroute("**/api/v1/environment-vault*")
    return `${answers} vault answers`
  })

  await step("the vault is created in the browser", async () => {
    await page.goto(`${origin}/environment`)
    await page.getByText("Create your vault", { exact: true }).waitFor(long)
    await page.locator("#vault-new-password").fill(secret)
    await page.locator("#vault-confirm-password").fill(secret)
    await button("Continue").click()
    await page.locator("code", { hasText: /^pbvr1-/ }).first().waitFor(long)
    await page.getByRole("checkbox").check()
    await button("Create vault").click()
    await button("Add variable").waitFor(long)
  })

  await step("a refused key rotation keeps its dialog with the error, the vault locks and unlocks again", async () => {
    await page.route("**/api/v1/environment/rotate-personal*", (route) =>
      route.request().method() === "POST"
        ? route.fulfill({ status: 400, json: { error: { code: "invalid_request", message: "stub: rotation refused" } } })
        : route.continue(),
    )
    await page.getByText("Personal key", { exact: true }).locator("xpath=ancestor::div[contains(@class,'border-b')][1]").getByRole("button", { name: "Replace…" }).click()
    const dialog = page.getByRole("dialog").or(page.getByRole("alertdialog"))
    await page.locator("#confirm-name").fill("ROTATE ENV")
    await dialog.getByRole("button", { name: "Replace key", exact: true }).click()
    await dialog.getByText("stub: rotation refused").waitFor(long)
    await page.waitForTimeout(1500)
    if (!(await dialog.getByText("stub: rotation refused").isVisible())) throw new Error("the dialog went away")
    await dialog.getByRole("button", { name: "Cancel", exact: true }).click()
    await page.getByLabel("Vault password").waitFor(long) // locked behind the dialog
    await page.unroute("**/api/v1/environment/rotate-personal*")
    await page.getByLabel("Vault password").fill(secret)
    await button("Unlock").click()
    await button("Add variable").waitFor(long)
  })
} catch (error) {
  if (!steps.some((s) => !s.ok)) console.log(`FAIL  setup  (${String(error.message ?? error).split("\n")[0]})`)
} finally {
  await browser.close()
}
const failed = steps.filter((s) => !s.ok).length
console.log(`${steps.length - failed}/${steps.length} passed`)
process.exit(failed || !steps.length ? 1 : 0)
