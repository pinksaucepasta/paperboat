// Headless check for F10-08 and the ENV team rows (console team vaults) on a live console, with TWO throwaway accounts that
// are both active members of one team (tools/device-smoke/fresh_account.mjs signup DIR twice; final_fixtures.py --email A
// --member B makes the team; A owns it). Neither account may have an ENV vault yet.
//   ORIGIN=<console origin> STATE_A=<owner's storage state> STATE_B=<member's storage state> PW_CHROMIUM=<chromium> \
//     [PW_MODULE=<playwright-core index.mjs>] node check-env-team-vault.mjs
// Both people set up their vault in the browser. Then: the owner creates the team vault and gives the member the key, the member
// accepts it and reads the owner's team variable, the owner removes the member (who loses the variable), replaces the team key,
// replaces the personal key (variables survive), resets the team vault (typed "RESET <team>"), and the member resets the personal
// vault (typed "RESET ENV", new password, old password dead, variables gone). Passwords are made here and never printed or sent.
import { randomBytes } from "node:crypto"
import { existsSync, readFileSync, writeFileSync } from "node:fs"

const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const { ORIGIN: origin, STATE_A, STATE_B, PW_CHROMIUM: executablePath } = process.env
const steps = []
const pass = (step, ok, detail = "") => {
  steps.push({ step, ok })
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const password = () => `pw-${randomBytes(9).toString("hex")}`
const long = { timeout: Number(process.env.LONG_MS ?? 90000) } // vault cryptography runs in WASM: minutes on a loaded host
const seen = []
const secrets = []
const people = []

const browser = await chromium.launch({ executablePath })

async function person(state, workspace) {
  const context = await browser.newContext({ storageState: state, viewport: { width: 1280, height: 1000 } })
  if (workspace) await context.addCookies([{ name: "pb-workspace", value: workspace, url: origin }])
  const page = await context.newPage()
  people.push(page)
  page.on("request", (request) => seen.push(`${request.url()}\n${request.postData() ?? ""}`))
  const button = (name) => page.getByRole("button", { name, exact: true })
  return {
    context,
    page,
    button,
    open: async () => {
      await page.goto(`${origin}/environment`)
      await page.waitForLoadState("networkidle").catch(() => {})
    },
    unlocked: () => button("Add variable").waitFor(long),
    /** Open after a page load: the vault is locked then, so unlock it with the password when the unlock card shows first. */
    ready: async (secret) => {
      const locked = page.getByLabel("Vault password")
      await locked.or(button("Add variable")).first().waitFor(long)
      if (await locked.isVisible()) {
        await locked.fill(secret)
        await button("Unlock").click()
      }
      await button("Add variable").waitFor(long)
    },
    locked: () => page.getByLabel("Vault password").waitFor(long),
    unlock: async (secret) => {
      await page.getByLabel("Vault password").fill(secret)
      await button("Unlock").click()
    },
    refused: () => page.getByText("ENV unlock failed").first().waitFor(long),
    row: (name) => page.locator("div.border-b", { has: page.locator("label", { hasText: name }) }).last(),
    dialog: () => page.getByRole("dialog").or(page.getByRole("alertdialog")),
    typeConfirm: async (text) => page.locator("#confirm-name").fill(text),
    variable: (name) => page.locator("code", { hasText: name }),
  }
}

async function step(name, run) {
  const started = Date.now()
  try {
    const detail = await run()
    pass(name, true, `${Math.round((Date.now() - started) / 1000)} s${typeof detail === "string" && detail ? `, ${detail}` : ""}`)
  } catch (error) {
    if (process.env.SHOT_DIR) await Promise.all(people.map((page, i) => page.screenshot({ path: `${process.env.SHOT_DIR}/fail-${i}.png`, fullPage: true }).catch(() => {})))
    pass(name, false, String(error.message ?? error).split("\n")[0].slice(0, 160))
    throw error
  }
}

/** Types once, then checks the text is still there a moment later: a form that clears what a person typed is a bug. */
async function typeOnce(locator, value) {
  await locator.fill(value)
  await locator.page().waitForTimeout(1500)
  if ((await locator.inputValue()) !== value) throw new Error("the typed text was cleared by the page")
}

/** A locked vault is started over from the unlock card: RESET ENV typed, a new password, no old password needed. */
async function resetLocked(who, fresh) {
  const dialog = who.dialog()
  await who.button("Reset vault…").click()
  await dialog.locator("#vault-new-password").fill(fresh)
  await dialog.locator("#vault-confirm-password").fill(fresh)
  if (await dialog.getByRole("button", { name: "Reset vault", exact: true }).isEnabled()) throw new Error("Reset is enabled before RESET ENV is typed")
  await dialog.locator("#reset-confirm").fill("RESET ENV")
  await dialog.getByRole("button", { name: "Reset vault", exact: true }).click()
  await dialog.waitFor({ state: "detached", ...long })
}

/** First-time setup in the browser: password, the one-time recovery code, confirmation. An account that already has a
 * vault whose password is not known (a rerun without PASSWORDS_FILE) is reset to the new password instead. */
async function setUp(who, secret, known) {
  const { page, button } = who
  await who.open()
  if (await page.getByLabel("Vault password").waitFor({ timeout: 8000 }).then(() => true, () => false)) {
    if (known) return who.unlock(secret).then(who.unlocked)
    await resetLocked(who, secret)
    await who.open()
    await who.locked()
    await who.unlock(secret)
    return who.unlocked()
  }
  await page.getByText("Create your vault", { exact: true }).waitFor(long)
  await typeOnce(page.locator("#vault-new-password"), secret)
  await typeOnce(page.locator("#vault-confirm-password"), secret)
  await button("Continue").click()
  await page.locator("code", { hasText: /^pbvr1-/ }).first().waitFor(long)
  await page.getByRole("checkbox").check()
  await button("Create vault").click()
  await who.unlocked()
}

async function addVariable(who, name, value) {
  await who.button("Add variable").click()
  await who.page.locator("#env-name").fill(name)
  await who.page.locator("#env-value").fill(value)
  await who.button("Save").click()
  await who.variable(name).waitFor(long)
}

try {
  const owner = await person(STATE_A)
  const member = await person(STATE_B)
  const team = await owner.page.goto(`${origin}/environment`).then(() =>
    owner.page.evaluate(async () => (await (await fetch("/api/v1/workspaces")).json()).data.items.find((w) => w.kind === "team")),
  )
  if (!team) throw new Error("the owner has no team: make it with final_fixtures.py")
  const rowOf = (who) => who.row(team.name)
  // PASSWORDS_FILE (mode 600, never printed) keeps the two passwords so a rerun on the same accounts unlocks instead of setting up.
  const file = process.env.PASSWORDS_FILE
  const kept = file && existsSync(file) ? JSON.parse(readFileSync(file, "utf8")) : undefined
  const [pwA, pwB] = kept ?? [password(), password()]
  if (file && !kept) writeFileSync(file, JSON.stringify([pwA, pwB]), { mode: 0o600 })
  secrets.push(pwA, pwB)
  const teamVar = `PB_TEAM_${randomBytes(3).toString("hex").toUpperCase()}`
  const personalVar = `PB_PERSONAL_${randomBytes(3).toString("hex").toUpperCase()}`

  // A response that lands before React hydrates must not change what the page renders (it caused a hydration mismatch
  // that remounted the setup form and wiped a typed password). The vault answer is stubbed instant, the CPU slowed.
  await step("the setup form of a new account hydrates cleanly and keeps what is typed", async () => {
    const fresh = await person(STATE_B)
    const errors = []
    fresh.page.on("pageerror", (error) => errors.push(String(error.message)))
    await fresh.page.route("**/api/v1/environment-vault*", (route) => route.fulfill({ status: 404, json: { error: { code: "not_found", message: "none" } } }))
    const cdp = await fresh.context.newCDPSession(fresh.page)
    await cdp.send("Emulation.setCPUThrottlingRate", { rate: 4 })
    await fresh.page.goto(`${origin}/environment`)
    await typeOnce(fresh.page.locator("#vault-new-password"), "not-a-real-password")
    await typeOnce(fresh.page.locator("#vault-confirm-password"), "not-a-real-password")
    await fresh.page.waitForTimeout(3000)
    if (errors.some((message) => message.includes("#418") || /hydrat/i.test(message))) throw new Error(`hydration error: ${errors[0].slice(0, 80)}`)
    await fresh.context.close()
  })

  await step("both members set up their own vault in the browser", async () => {
    await setUp(owner, pwA, Boolean(kept))
    await setUp(member, pwB, Boolean(kept))
  })

  // RESUME=1 continues a run on accounts whose team vault a failed run already created (the evidence run is never resumed).
  const resumed = process.env.RESUME === "1"
  await step("the owner's Team vaults section offers Create and the member cannot create", async () => {
    if (resumed) return "skipped: resumed"
    await owner.open()
    await owner.ready(pwA)
    await rowOf(owner).getByText("No variables yet.").waitFor(long)
    await rowOf(owner).getByRole("button", { name: "Create…" }).waitFor(long)
    await member.open()
    await rowOf(member).getByText("No variables yet.").waitFor(long)
    if (await rowOf(member).getByRole("button").count()) throw new Error("a plain member is offered team vault buttons")
  })

  await step("the owner creates the team vault: key in use", async () => {
    if (resumed) return "skipped: resumed"
    await rowOf(owner).getByRole("button", { name: "Create…" }).click()
    await owner.dialog().getByRole("button", { name: "Create", exact: true }).click()
    await rowOf(owner).getByRole("button", { name: "Give key…" }).waitFor(long)
    await rowOf(owner).getByText("Key in use.").waitFor(long)
  })

  await step("the owner lets members read team variables (access policy), then the row says a member needs the key", async () => {
    await owner.page.evaluate(async (id) => {
      const url = `/api/v1/workspaces/${encodeURIComponent(id)}/policy`
      const current = (await (await fetch(url)).json()).data
      const file = JSON.parse(current.policy.replace(/\/\/.*$/gm, "").replace(/,(\s*[}\]])/g, "$1"))
      file.rules = [...(file.rules ?? []), { src: ["role:member"], dst: ["env:team"], allow: ["env_read"] }]
      // A save that changes who can do what is answered 412 with a confirm token; it is sent back, as the console's dialog does.
      const save = (confirm) =>
        fetch(url, {
          method: "PUT",
          headers: { "content-type": "application/json", "idempotency-key": crypto.randomUUID() },
          body: JSON.stringify({ policy: JSON.stringify(file), expected_version: current.version, ...(confirm && { confirm }) }),
        })
      let saved = await save()
      if (saved.status === 412) saved = await save((await saved.json()).error.details.confirm)
      if (!saved.ok) throw new Error(`policy save answered ${saved.status} ${(await saved.text()).slice(0, 160)}`)
    }, team.id)
    await owner.open()
    await owner.ready(pwA)
    await rowOf(owner).getByText(/1 member needs the key/).waitFor(long)
  })

  await step("the owner gives the member the team key", async () => {
    await rowOf(owner).getByRole("button", { name: "Give key…" }).click()
    const dialog = owner.dialog()
    if ((await dialog.getByRole("button", { name: "Give key", exact: true }).count()) !== 1) throw new Error("expected exactly the member")
    await dialog.getByRole("button", { name: "Give key", exact: true }).click()
    await dialog.waitFor({ state: "detached", ...long })
    await rowOf(owner).getByText("Key in use.").waitFor(long)
  })

  await step("the owner adds a team variable in the team workspace", async () => {
    await owner.context.addCookies([{ name: "pb-workspace", value: team.id, url: origin }])
    await owner.open()
    await owner.ready(pwA)
    await addVariable(owner, teamVar, `value-${randomBytes(6).toString("hex")}`)
  })

  await step("the member accepts the key and then reads the team variable", async () => {
    await member.open()
    await member.ready(pwB)
    await member.button("Accept keys").click()
    await member.page.getByText("1 accepted.").waitFor(long)
    await member.context.addCookies([{ name: "pb-workspace", value: team.id, url: origin }])
    await member.open()
    await member.ready(pwB)
    await member.variable(teamVar).waitFor(long)
  })

  await step("the owner removes the member's access (typed team id) and the member loses the variable", async () => {
    await owner.open()
    await owner.ready(pwA)
    await rowOf(owner).getByRole("button", { name: "Remove access…" }).click()
    const dialog = owner.dialog()
    await dialog.getByRole("textbox").fill(team.id)
    await dialog.getByRole("button", { name: "Remove", exact: true }).click()
    await dialog.waitFor({ state: "detached", ...long })
    await owner.variable(teamVar).waitFor(long)
    await member.open()
    await member.ready(pwB)
    await member.page.waitForTimeout(3000)
    if (await member.variable(teamVar).count()) throw new Error("the removed member still lists the team variable")
  })

  await step("the owner replaces the team key (typed team id): the variable is still there", async () => {
    await rowOf(owner).getByRole("button", { name: "New key…" }).click()
    await owner.typeConfirm(team.id)
    await owner.dialog().getByRole("button", { name: "Replace key", exact: true }).click()
    await owner.dialog().waitFor({ state: "detached", ...long })
    await rowOf(owner).getByText("Key in use.").waitFor(long)
    await owner.open()
    await owner.ready(pwA)
    await owner.variable(teamVar).waitFor(long)
  })

  await step("the personal key is replaced (typed ROTATE ENV): the variable and the password still work", async () => {
    await owner.context.addCookies([{ name: "pb-workspace", value: "personal", url: origin }])
    await owner.open()
    await owner.ready(pwA)
    await addVariable(owner, personalVar, `value-${randomBytes(6).toString("hex")}`)
    await owner.page.getByText("Personal key", { exact: true }).locator("xpath=ancestor::div[contains(@class,'border-b')][1]").getByRole("button", { name: "Replace…" }).click()
    await owner.typeConfirm("ROTATE ENV")
    await owner.dialog().getByRole("button", { name: "Replace key", exact: true }).click()
    await owner.dialog().waitFor({ state: "detached", ...long })
    await owner.button("Lock").click()
    await owner.locked()
    await owner.unlock(pwA)
    await owner.unlocked()
    await owner.variable(personalVar).waitFor(long)
  })

  await step("the owner resets the team vault (typed RESET <team>): the team variable is gone", async () => {
    await owner.context.addCookies([{ name: "pb-workspace", value: team.id, url: origin }])
    await owner.open()
    await owner.ready(pwA)
    await owner.variable(teamVar).waitFor(long)
    await rowOf(owner).getByRole("button", { name: "Reset…" }).click()
    await owner.typeConfirm(`RESET ${team.id}`)
    await owner.dialog().getByRole("button", { name: "Reset", exact: true }).click()
    await owner.dialog().waitFor({ state: "detached", ...long })
    await owner.page.waitForTimeout(3000)
    await owner.open()
    await owner.ready(pwA)
    if (await owner.variable(teamVar).count()) throw new Error("the team variable survived the reset")
  })

  await step("a locked vault is reset with RESET ENV and a new password: old password dead, variables gone", async () => {
    await member.context.addCookies([{ name: "pb-workspace", value: "personal", url: origin }])
    await member.open()
    await member.ready(pwB)
    await addVariable(member, personalVar, `value-${randomBytes(6).toString("hex")}`)
    await member.button("Lock").click()
    await member.locked()
    const fresh = password()
    secrets.push(fresh)
    await resetLocked(member, fresh)
    await member.open()
    await member.locked()
    await member.unlock(pwB)
    await member.refused()
    await member.unlock(fresh)
    await member.unlocked()
    await member.page.waitForTimeout(2000)
    if (await member.variable(personalVar).count()) throw new Error("the personal variable survived the reset")
  })

  await step("no password was in any request the page sent", async () => {
    const all = seen.join("\n")
    const leaked = secrets.filter((secret) => all.includes(secret) || all.includes(encodeURIComponent(secret)))
    if (leaked.length) throw new Error(`${leaked.length} secret(s) were sent`)
  })
} catch (error) {
  if (!steps.some((s) => !s.ok)) console.log(`FAIL  setup  (${String(error.message ?? error).split("\n")[0]})`)
} finally {
  await browser.close()
}
const failed = steps.filter((s) => !s.ok).length
console.log(`${steps.length - failed}/${steps.length} passed`)
process.exit(failed || !steps.length ? 1 : 0)
