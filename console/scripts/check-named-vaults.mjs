// Headless check for F26-04 / F10-08 (named ENV vaults on the Environment page) on a live console, with one throwaway account
// (tools/device-smoke/fresh_account.mjs signup DIR). Sets up its ENV vault in the browser, then: the switcher lists Default,
// New vault makes a named vault, a variable is added (its value never reaches the server), the vault and Default show
// their own variables, the variable is removed, the vault is deleted after its name is typed. Nothing secret is printed.
//   ORIGIN=<console origin> STATE=<storage state> PW_CHROMIUM=<chromium> [PW_MODULE=<playwright-core index.mjs>] \
//     [PASSWORD_FILE=<mode 600 file keeping the ENV password for reruns>] [SHOT_DIR=<dir>] node check-named-vaults.mjs
import { randomBytes } from "node:crypto"
import { existsSync, readFileSync, writeFileSync } from "node:fs"

const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const { ORIGIN: origin, STATE, PW_CHROMIUM: executablePath, PASSWORD_FILE } = process.env
for (const [name, value] of Object.entries({ ORIGIN: origin, STATE, PW_CHROMIUM: executablePath }))
  if (!value) throw new Error(`${name} is not set; see the header of check-named-vaults.mjs`)
const long = { timeout: Number(process.env.LONG_MS ?? 90000) } // vault cryptography runs in WASM
const kept = PASSWORD_FILE && existsSync(PASSWORD_FILE) ? readFileSync(PASSWORD_FILE, "utf8").trim() : undefined
const password = kept ?? `pw-${randomBytes(9).toString("hex")}`
if (PASSWORD_FILE && !kept) writeFileSync(PASSWORD_FILE, password, { mode: 0o600 })
const suffix = randomBytes(3).toString("hex")
const vault = `check-${suffix}`
const variable = `PB_VAULT_${suffix.toUpperCase()}`
const secret = `value-${randomBytes(9).toString("hex")}`

const browser = await chromium.launch({ executablePath })
const context = await browser.newContext({ storageState: STATE, viewport: { width: 1280, height: 1000 } })
const page = await context.newPage()
const sent = []
page.on("request", (request) => sent.push(`${request.url()}\n${request.postData() ?? ""}`))
const button = (name) => page.getByRole("button", { name, exact: true })
const dialog = () => page.getByRole("dialog").or(page.getByRole("alertdialog"))
const api = (path, init) =>
  context.request.fetch(`${origin}/api/v1/${path}`, { ...init, headers: { origin, ...init?.headers } })
let failed = 0

async function step(name, run) {
  const started = Date.now()
  try {
    const detail = await run()
    console.log(`PASS  ${name}  (${Math.round((Date.now() - started) / 1000)} s${detail ? `, ${detail}` : ""})`)
  } catch (error) {
    failed++
    if (process.env.SHOT_DIR)
      await page.screenshot({ path: `${process.env.SHOT_DIR}/fail-${failed}.png`, fullPage: true }).catch(() => {})
    console.log(
      `FAIL  ${name}  (${String(error.message ?? error)
        .split("\n")[0]
        .slice(0, 160)})`,
    )
  }
}
const check = (ok, message) => {
  if (!ok) throw new Error(message)
}
const pressed = (name) => button(name).getAttribute("aria-pressed")

try {
  await step("ENV is set up in the browser (or unlocked) and the Environment page opens", async () => {
    await page.goto(`${origin}/environment`)
    const locked = page.getByLabel("Vault password")
    const fresh = page.getByText("Create your vault", { exact: true })
    await locked.or(fresh).or(button("Add variable")).first().waitFor(long)
    if (await fresh.isVisible()) {
      await page.locator("#vault-new-password").fill(password)
      await page.locator("#vault-confirm-password").fill(password)
      await button("Continue").click()
      await page
        .locator("code", { hasText: /^pbvr1-/ })
        .first()
        .waitFor(long)
      await page.getByRole("checkbox").check()
      await button("Create vault").click()
    } else if (await locked.isVisible()) {
      await locked.fill(password)
      await button("Unlock").click()
    }
    await button("Add variable").waitFor(long)
  })
  await step("the switcher shows Default selected and New vault", async () => {
    check((await pressed("Default")) === "true", "Default is not selected")
    await button("New vault").waitFor()
  })
  await step("New vault makes a named vault, which is selected and empty", async () => {
    await button("New vault").click()
    await page.locator("#vault-name").fill(vault)
    await dialog().getByRole("button", { name: "Create", exact: true }).click()
    await dialog().waitFor({ state: "detached", ...long })
    await page.getByRole("heading", { name: `Vault ${vault}` }).waitFor(long)
    check((await pressed(vault)) === "true", "the new vault is not selected")
    await page.getByText("No variables yet.").waitFor(long)
    await page.getByText("No recipe reads this vault.").waitFor()
    const made = await api(`workspaces/personal/env-vaults/${vault}`)
    check(made.status() === 200, `the server answers ${made.status()} for the vault`)
  })
  await step("a variable added to the vault is listed, and its value never reaches the server", async () => {
    await button("Add variable").click()
    await page.locator("#env-name").fill(variable)
    await page.locator("#env-value").fill(secret)
    await button("Save").click()
    await page.locator("code", { hasText: variable }).waitFor(long)
    check(!sent.some((request) => request.includes(secret)), "the value was sent in the clear")
    const stored = await (await api(`workspaces/personal/env-vaults/${vault}`)).json()
    check(
      stored.data.vault.revision >= 1 && !JSON.stringify(stored).includes(secret),
      "the server's copy is not sealed",
    )
  })
  await step("Default shows its own variables and the vault keeps its own", async () => {
    await button("Default").click()
    await page.getByRole("heading", { name: /^(.* variables|Variables)$/ }).waitFor(long)
    await page.locator("code", { hasText: variable }).waitFor({ state: "detached", ...long })
    await button(vault).click()
    await page.locator("code", { hasText: variable }).waitFor(long)
  })
  await step("a variable is removed from the vault", async () => {
    await page.getByRole("button", { name: `Remove ${variable}` }).click()
    await dialog().getByRole("button", { name: "Remove", exact: true }).click()
    await page.getByText("No variables yet.").waitFor(long)
  })
  await step("the vault is deleted after its name is typed, and Default is shown again", async () => {
    await button("Delete").click()
    const confirm = dialog().getByRole("button", { name: "Delete", exact: true })
    check(await confirm.isDisabled(), "Delete is enabled before the name is typed")
    await dialog().locator("#confirm-name").fill(vault)
    await confirm.click()
    await dialog().waitFor({ state: "detached", ...long })
    await button(vault).waitFor({ state: "detached", ...long })
    check((await pressed("Default")) === "true", "Default is not selected after the delete")
    check((await api(`workspaces/personal/env-vaults/${vault}`)).status() === 404, "the server still has the vault")
  })
} finally {
  await api(`workspaces/personal/env-vaults/${vault}`, { method: "DELETE" }).catch(() => {})
  await browser.close()
}
console.log(failed ? `${failed} failed` : "all passed")
process.exit(failed ? 1 : 0)
