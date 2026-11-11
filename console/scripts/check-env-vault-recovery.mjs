// Headless check for F10-07 (console vault setup, recovery code, recovery) on a live console, signed in as a
// THROWAWAY account that has no ENV vault yet (tools/device-smoke/fresh_account.mjs signup DIR makes one).
//   ORIGIN=<console origin> STATE=<the account's Playwright storage state> PW_CHROMIUM=<chromium> \
//     [PW_MODULE=<playwright-core index.mjs>] node check-env-vault-recovery.mjs
// Walks /environment: creates the vault in the browser (password + recovery code shown once), adds a variable,
// recovers a locked vault with the code (new password, new code, old code dead, the variable still there),
// changes the password, replaces the code, turns it off and on again. Passwords and codes are made here, never
// printed, and must appear in no request the page sends. Prints PASS/FAIL per step; exits 1 when one failed.
import { randomBytes } from "node:crypto"

const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const { ORIGIN: origin, STATE: state, PW_CHROMIUM: executablePath } = process.env
const steps = []
const pass = (step, ok, detail = "") => {
  steps.push({ step, ok })
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const password = () => `pw-${randomBytes(9).toString("hex")}`
const seen = []

const browser = await chromium.launch({ executablePath })
const context = await browser.newContext({ storageState: state, viewport: { width: 1280, height: 900 } })
const page = await context.newPage()
page.on("request", (request) => seen.push(`${request.url()}\n${request.postData() ?? ""}`))
const long = { timeout: 90000 }
const button = (name) => page.getByRole("button", { name, exact: true })
const body = () => page.locator("body").innerText()
const html = () => page.content()
const shownCode = async () => (await page.locator("code", { hasText: /^pbvr1-/ }).first().innerText({ timeout: 30000 })).trim()
const secrets = []

async function step(name, run) {
  try {
    const detail = await run()
    pass(name, true, typeof detail === "string" ? detail : "")
  } catch (error) {
    pass(name, false, String(error.message ?? error).split("\n")[0].slice(0, 160))
    throw error
  }
}

/** Ticks "I saved my recovery code" and presses the step's button. */
async function confirmSaved(label) {
  await page.getByRole("checkbox").check()
  await button(label).click()
}
const refused = () => page.getByText("ENV unlock failed").first().waitFor(long)
const unlocked = () => button("Add variable").waitFor(long)
const lockedCard = () => page.getByLabel("Vault password").waitFor(long)
async function unlockWith(secret) {
  await page.getByLabel("Vault password").fill(secret)
  await button("Unlock").click()
}
async function fillPasswords(secret) {
  await page.locator("#vault-new-password").fill(secret)
  await page.locator("#vault-confirm-password").fill(secret)
}

try {
  await page.goto(`${origin}/environment`)
  await page.waitForLoadState("networkidle").catch(() => {})

  let first = password()
  let code = ""
  await step("a fresh account is offered setting up the vault in the browser", async () => {
    await page.getByText("Create your vault", { exact: true }).waitFor(long)
    if (await page.getByText("pb env vault init").count()) throw new Error("still sends people to the CLI")
  })
  await step("mismatched passwords are refused before anything is made", async () => {
    await page.locator("#vault-new-password").fill(first)
    await page.locator("#vault-confirm-password").fill("different")
    await page.getByText("The passwords don’t match.").waitFor()
    if (await button("Continue").isEnabled()) throw new Error("Continue is enabled")
  })
  await step("setup shows the recovery code once and waits for the confirmation", async () => {
    await fillPasswords(first)
    secrets.push(first)
    await button("Continue").click()
    code = await shownCode()
    secrets.push(code)
    if (!/^pbvr1-[A-Z2-7]{58}$/.test(code)) throw new Error("not a recovery code")
    if (await button("Create vault").isEnabled()) throw new Error("Create vault is enabled before the code is saved")
    if ((await page.locator("#vault-new-password").count()) !== 0) throw new Error("password still on the page")
  })
  await step("the vault is created and open, with the recovery code on", async () => {
    await confirmSaved("Create vault")
    await unlocked()
    await page.getByText("Recovery code", { exact: true }).waitFor(long)
    await page.getByRole("button", { name: "Replace…" }).waitFor(long)
    if ((await body()).includes(code)) throw new Error("the code is still on the page")
  })

  await step("a variable can be set and listed", async () => {
    await button("Add variable").click()
    await page.locator("#env-name").fill("PB_VAULT_RECOVERY_CHECK")
    await page.locator("#env-value").fill(`value-${randomBytes(6).toString("hex")}`)
    await button("Save").click()
    await page.locator("code", { hasText: "PB_VAULT_RECOVERY_CHECK" }).waitFor(long)
  })

  await step("locking brings back the unlock card with the recovery link", async () => {
    await button("Lock").click()
    await lockedCard()
    await page.getByRole("button", { name: "Forgot password? Use your recovery code" }).waitFor()
  })

  await step("a wrong recovery code is refused and nothing opens", async () => {
    await page.getByRole("button", { name: "Forgot password? Use your recovery code" }).click()
    await page.locator("#vault-recovery-code").fill("pbvr1-" + "A".repeat(58))
    await fillPasswords(password())
    await button("Continue").click()
    await refused()
  })
  await step("cancelling recovery returns to the unlock card, still locked", async () => {
    await button("Cancel").click()
    await lockedCard()
    if (await button("Add variable").count()) throw new Error("the vault opened")
  })

  const second = password()
  secrets.push(second)
  let next = ""
  await step("the recovery code opens the vault in the browser and a new code is shown first", async () => {
    await page.getByRole("button", { name: "Forgot password? Use your recovery code" }).click()
    await page.locator("#vault-recovery-code").fill(code)
    await fillPasswords(second)
    await button("Continue").click()
    next = await shownCode()
    secrets.push(next)
    if (next === code) throw new Error("the new code equals the old one")
    if (await button("Finish").isEnabled()) throw new Error("Finish is enabled before the code is saved")
    if (await button("Add variable").count()) throw new Error("the vault opened before the new password was published")
  })
  await step("finishing recovery opens the vault with the same variables", async () => {
    await confirmSaved("Finish")
    await unlocked()
    await page.locator("code", { hasText: "PB_VAULT_RECOVERY_CHECK" }).waitFor(long)
    const text = await body()
    if (text.includes(next) || text.includes(code) || text.includes(second)) throw new Error("a secret is on the page")
  })

  await step("the old password and the old code no longer unlock", async () => {
    await button("Lock").click()
    await lockedCard()
    await unlockWith(first)
    await refused()
    await page.getByRole("button", { name: "Forgot password? Use your recovery code" }).click()
    await page.locator("#vault-recovery-code").fill(code)
    await fillPasswords(password())
    await button("Continue").click()
    await refused()
    await button("Cancel").click()
    await lockedCard()
  })
  await step("the new password unlocks", async () => {
    await unlockWith(second)
    await unlocked()
  })

  const third = password()
  secrets.push(third)
  await step("the password is changed from the vault settings", async () => {
    await button("Change…").click()
    await fillPasswords(third)
    await page.getByRole("dialog").getByRole("button", { name: "Change password", exact: true }).click()
    await page.getByRole("dialog").waitFor({ state: "detached", ...long })
    await button("Lock").click()
    await lockedCard()
    await unlockWith(second)
    await refused()
    await unlockWith(third)
    await unlocked()
  })

  await step("the recovery code is replaced: the new code is shown first, the old one stops working", async () => {
    await page.getByRole("button", { name: "Replace…" }).click()
    const replacement = await shownCode()
    secrets.push(replacement)
    await page.getByRole("dialog").getByRole("checkbox").check()
    await page.getByRole("dialog").getByRole("button", { name: "Replace", exact: true }).click()
    await page.getByRole("dialog").waitFor({ state: "detached", ...long })
    await button("Lock").click()
    await lockedCard()
    await page.getByRole("button", { name: "Forgot password? Use your recovery code" }).click()
    await page.locator("#vault-recovery-code").fill(next)
    await fillPasswords(password())
    await button("Continue").click()
    await refused()
    await button("Cancel").click()
    await lockedCard()
    await unlockWith(third)
    await unlocked()
  })

  await step("turning the recovery code off asks first and leaves no recovery path", async () => {
    await page.getByRole("button", { name: "Turn off…" }).click()
    await page.getByRole("alertdialog").getByText("can’t be recovered").waitFor()
    await page.getByRole("alertdialog").getByRole("button", { name: "Turn off", exact: true }).click()
    await page.getByRole("button", { name: "Create…" }).waitFor(long)
    await page.getByText("Off. If you forget your password").waitFor()
  })
  await step("a recovery code can be created again", async () => {
    await page.getByRole("button", { name: "Create…" }).click()
    const created = await shownCode()
    secrets.push(created)
    await page.getByRole("dialog").getByRole("checkbox").check()
    await page.getByRole("dialog").getByRole("button", { name: "Create", exact: true }).click()
    await page.getByRole("button", { name: "Replace…" }).waitFor(long)
  })

  await step("the variable is still there and can be removed", async () => {
    await page.locator("code", { hasText: "PB_VAULT_RECOVERY_CHECK" }).waitFor(long)
    await page.getByRole("button", { name: "Remove PB_VAULT_RECOVERY_CHECK" }).click()
    await page.getByRole("alertdialog").getByRole("button", { name: "Remove", exact: true }).click()
    await page.locator("code", { hasText: "PB_VAULT_RECOVERY_CHECK" }).waitFor({ state: "detached", ...long })
  })

  await step("no password or recovery code was in any request the page sent", async () => {
    const all = seen.join("\n")
    const leaked = secrets.filter((secret) => all.includes(secret) || all.includes(encodeURIComponent(secret)))
    if (leaked.length) throw new Error(`${leaked.length} secret(s) were sent`)
    if ((await html()).includes(secrets[0])) throw new Error("a password is in the page")
  })
} catch {
  // the failing step is already reported
} finally {
  await browser.close()
}
const failed = steps.filter((s) => !s.ok).length
console.log(`${steps.length - failed}/${steps.length} passed`)
process.exit(failed || !steps.length ? 1 : 0)
