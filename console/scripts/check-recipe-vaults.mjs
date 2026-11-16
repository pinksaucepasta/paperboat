// Recipes that read named ENV vaults, in the console (F26, W-08 types): a recipe naming a vault the workspace lacks
// offers "Create vault" (the vault engine wraps the new key), needs ENV unlocked, is created with its token's
// public key and the vault's key wrapped to it, can be edited, and its rotation wraps the vault to the new key.
//   STATE=<storage state of the account> CHROME=<Chromium> CONSOLE_ORIGIN=<console origin>
//   VAULT_PASSWORD_FILE=<file holding that account's ENV vault password> [PW_MODULE=<playwright-core index.mjs>]
//   node check-recipe-vaults.mjs
// Works in the personal workspace through the console's own pages (no full page load once ENV is unlocked, since a
// reload locks it). Makes one recipe and one vault named check-<random> and removes both. Nothing secret is printed.
import { readFileSync } from "node:fs"

const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const { STATE, CHROME, CONSOLE_ORIGIN: origin, VAULT_PASSWORD_FILE } = process.env
for (const [name, value] of Object.entries({ STATE, CHROME, CONSOLE_ORIGIN: origin, VAULT_PASSWORD_FILE }))
  if (!value) throw new Error(`${name} is not set; see the header of check-recipe-vaults.mjs`)
const browser = await chromium.launch({ executablePath: CHROME })
const ctx = await browser.newContext({ storageState: STATE, viewport: { width: 1280, height: 900 } })
const page = await ctx.newPage()
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const api = async (path, init) => {
  const response = await ctx.request.fetch(`${origin}/api/v1/${path}`, init)
  const json = await response.json().catch(() => ({}))
  return { status: response.status(), data: json.data, error: json.error }
}
const name = `check-${Math.random().toString(36).slice(2, 8)}`
const vault = `v-${name}`
const recipes = "workspaces/personal/recipes"
const body = (extra = "") => `tags = ["check"]\nmax_machines = 2\n\n[env]\nvaults = ["${vault}"]\n${extra}`

try {
  await page.goto(`${origin}/settings/recipes`, { waitUntil: "networkidle", timeout: 120000 })
  await page.getByRole("link", { name: "New recipe" }).first().click()
  await page.locator("#recipe-name").fill(name)
  await page.locator("#recipe-file").fill(body())
  await page.getByRole("button", { name: "Create recipe" }).click()
  const offer = page.getByRole("button", { name: `Create vault ${vault}` })
  await offer.waitFor({ timeout: 30000 })
  log(true, "a recipe naming a vault the workspace lacks offers to create it")
  log(!(await api(`${recipes}/${name}`)).data, "nothing was created yet")

  await offer.click()
  const unlock = page.locator('input[name="vault-password"]')
  await unlock.waitFor({ timeout: 30000 })
  log(true, "creating the vault asks to unlock ENV first")
  await unlock.fill(readFileSync(VAULT_PASSWORD_FILE, "utf8").trim())
  await page.getByRole("button", { name: "Unlock", exact: true }).click()
  await page.getByText("Unlocked. Save again.").waitFor({ timeout: 60000 })
  log(true, "unlocking says so")

  await offer.click()
  const token = page.locator("[data-recipe-token]")
  await token.waitFor({ timeout: 60000 })
  log(/^pbr_[a-z2-7]{10}_[a-z2-7]{52}$/.test((await token.textContent()).trim()), "the vault is made, the recipe is created and its token shown once")
  const made = await api(`workspaces/personal/env-vaults/${vault}`)
  log(made.status === 200 && made.data.vault.name === vault && made.data.vault.wraps.some((w) => w.recipient_kind === "account"), "the server has the vault, wrapped for the account")
  log(made.data?.vault.recipes.includes(name), "the vault lists the recipe as a reader (its key was wrapped to the recipe)")
  const first = (await api(`${recipes}/${name}`)).data.recipe
  log(first.revision === 1 && Buffer.from(first.public_key ?? "", "base64").length === 32, "the recipe stores its token's public key", `revision ${first.revision}`)

  await page.getByRole("button", { name: "I saved it" }).click()
  await page.getByRole("link", { name: "Edit", exact: true }).first().click()
  await page.locator("#recipe-file").fill(body("# edited in the console\n"))
  await page.getByRole("button", { name: "Save changes" }).click()
  await page.waitForURL((url) => url.pathname === `/settings/recipes/${name}`, { timeout: 60000 })
  const edited = (await api(`${recipes}/${name}`)).data.recipe
  log(edited.revision === 2 && edited.source.includes("edited in the console"), "an edit that keeps the vault saves at revision 2", `revision ${edited.revision}`)

  await page.getByRole("button", { name: "Rotate token" }).first().click()
  const dialog = page.locator("[role=dialog], [role=alertdialog]")
  const code = (await dialog.locator("label span.font-mono").textContent({ timeout: 30000 })).trim()
  await dialog.locator("input").fill(code)
  await dialog.getByRole("button", { name: "Rotate token" }).click()
  await dialog.locator("[data-recipe-token]").waitFor({ timeout: 60000 })
  const text = await dialog.innerText()
  log(!/vault keys were not wrapped/.test(text), "rotating wraps the vault to the new token's key", /not wrapped/.test(text) ? text.slice(0, 200) : "")
  const rotated = (await api(`${recipes}/${name}`)).data.recipe
  log(rotated.public_key && rotated.public_key !== first.public_key, "the recipe's public key changed with its token")
  await dialog.getByRole("button", { name: "I saved it" }).click()
} finally {
  await api(`${recipes}/${name}`).then(async ({ data }) => {
    if (!data) return
    const asked = await api(`${recipes}/${name}/revoke`, { method: "POST", data: {}, headers: { "content-type": "application/json" } })
    const confirm = asked.error?.details?.confirm
    if (confirm) await api(`${recipes}/${name}/revoke`, { method: "POST", data: { confirm }, headers: { "content-type": "application/json" } })
  })
  const gone = await api(`workspaces/personal/env-vaults/${vault}`, { method: "DELETE" })
  log([200, 204, 404].includes(gone.status), "the check's recipe and vault are removed", `vault delete HTTP ${gone.status}`)
}
await browser.close()
console.log(failed ? `${failed} failed` : "all passed")
process.exit(failed ? 1 : 0)
