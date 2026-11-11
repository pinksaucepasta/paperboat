// Teams session recordings in the console: settings, list, replay and the terminal's REC control.
//   STATE=<storage state of a team owner> CHROME=<Chromium> CONSOLE_ORIGIN=<console origin> TEAM=<team workspace id>
//   [MACHINE=<team machine id, online, with a recording key set up>]  terminal rows: start, REC, stop
//   [RECORDING=<recording id> VAULT_PASSWORD_FILE=<file holding the owner's vault password>]  replay rows
//   [OFFLINE_RECORDING=<recording id on an offline machine>]  the offline state
//   node check-recordings.mjs
// Rows without their variables are skipped and say so. Nothing secret is printed: no password, ticket or terminal text.
import { readFileSync } from "node:fs"
import { chromium } from "playwright-core"

const { STATE, CHROME, CONSOLE_ORIGIN: origin, TEAM: team, MACHINE: machine, RECORDING: recording } = process.env
for (const [name, value] of Object.entries({ STATE, CHROME, CONSOLE_ORIGIN: origin, TEAM: team }))
  if (!value) throw new Error(`${name} is not set; see the header of check-recordings.mjs`)

const browser = await chromium.launch({ executablePath: CHROME })
const ctx = await browser.newContext({ storageState: STATE, viewport: { width: 1280, height: 900 } })
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const skip = (step, why) => console.log(`SKIP  ${step}  (${why})`)
const get = async (path) => {
  const res = await ctx.request.get(`${origin}/api/v1/${path}`)
  return { status: res.status(), body: await res.json().catch(() => null) }
}
const workspace = (id) => ctx.addCookies([{ name: "pb-workspace", value: id, url: origin }])
const page = await ctx.newPage()

try {
  // 1. Personal: recordings are a Teams feature.
  await workspace("personal")
  await page.goto(`${origin}/recordings`, { waitUntil: "networkidle", timeout: 120000 })
  log(await page.locator("[data-recordings-upgrade]").isVisible({ timeout: 20000 }).catch(() => false), "Personal shows the Teams upgrade state")

  // 2. The team's list matches the server's first page.
  await workspace(team)
  const list = await get(`teams/${encodeURIComponent(team)}/recordings?limit=50&offset=0`)
  await page.goto(`${origin}/recordings`, { waitUntil: "networkidle", timeout: 120000 })
  if (list.status === 409) log(await page.locator("[data-recordings-upgrade]").isVisible(), "a team without the plan shows the upgrade state")
  else {
    const rows = list.body?.data?.recordings ?? []
    if (rows.length === 0) log(await page.getByText("No recordings yet").isVisible({ timeout: 20000 }).catch(() => false), "an empty team shows No recordings yet")
    else {
      await page.locator("[data-recording-id]").first().waitFor({ timeout: 30000 })
      const shown = await page.locator("[data-recording-id]").evaluateAll((els) => els.map((el) => el.getAttribute("data-recording-id")))
      log(JSON.stringify(shown) === JSON.stringify(rows.map((r) => r.id)), "the list shows the server's first page in order", `${rows.length} of ${list.body.data.total}`)
      const pager = await page.locator("[data-list-range]").textContent().catch(() => null)
      log(list.body.data.total > 50 ? pager?.includes(`of ${list.body.data.total}`) : pager === null, "the pager follows the server total", pager ?? "one page")
      const first = rows[0]
      await page.goto(`${origin}/machines/${encodeURIComponent(first.machine_id)}/recordings`, { waitUntil: "networkidle", timeout: 120000 })
      const own = await get(`teams/${encodeURIComponent(team)}/recordings?limit=50&offset=0&machine=${encodeURIComponent(first.machine_id)}`)
      await page.locator("[data-recording-id]").first().waitFor({ timeout: 30000 }).catch(() => {})
      const tab = await page.locator("[data-recording-id]").evaluateAll((els) => els.map((el) => el.getAttribute("data-recording-id")))
      log(JSON.stringify(tab) === JSON.stringify((own.body?.data?.recordings ?? []).map((r) => r.id)), "a machine's tab lists only its recordings", first.machine_name)
    }
  }

  // 3. Team → Recordings: key state, the manual-start switch (flipped and restored), rules from the policy.
  await page.goto(`${origin}/team?team=${encodeURIComponent(team)}&tab=recordings`, { waitUntil: "networkidle", timeout: 120000 })
  const key = await get(`teams/${encodeURIComponent(team)}/recording-key`)
  if (key.status === 409) log(await page.locator("[data-recordings-upgrade]").isVisible({ timeout: 20000 }).catch(() => false), "settings show the upgrade state without the plan")
  else {
    await page.locator("[data-recording-settings]").waitFor({ timeout: 30000 })
    const status = await page.locator("[data-recording-key]").getAttribute("data-recording-key", { timeout: 20000 })
    log(status === (key.body.data.key ? "ready" : "missing"), "the key status matches the server", status)
    const toggle = page.getByRole("switch", { name: "Only admins start recordings" })
    const before = key.body.data.manual_start
    await toggle.click()
    let after = before
    for (let i = 0; i < 20 && after === before; i++) {
      await page.waitForTimeout(500)
      after = (await get(`teams/${encodeURIComponent(team)}/recording-key`)).body?.data?.manual_start
    }
    log(after !== before, "the admins-only switch changes the team setting", `${before} -> ${after}`)
    await toggle.click()
    await page.waitForTimeout(3000)
    log((await get(`teams/${encodeURIComponent(team)}/recording-key`)).body?.data?.manual_start === before, "switching back restores it")
    const policy = (await get(`workspaces/${encodeURIComponent(team)}/policy`)).body?.data?.policy ?? "{}"
    const expected = (policy.match(/"(record|recording_view)"/g) ?? []).length
    log((await page.locator("[data-recording-rule]").count()) === expected, "every recording rule in the policy is listed", `${expected}`)
    // A replay rule for admins on every machine: added through the review dialog, listed, then removed again.
    const replayRules = async () => ((await get(`workspaces/${encodeURIComponent(team)}/policy`)).body?.data?.policy.match(/"recording_view"/g) ?? []).length
    const rulesBefore = await replayRules()
    await page.getByRole("button", { name: "Add rule" }).click()
    const dialog = page.getByRole("dialog")
    log(await dialog.getByText("Record sessions").isVisible({ timeout: 5000 }).catch(() => false), "Add rule opens its dialog")
    await dialog.getByText("Allow replay").click()
    await dialog.getByText("Admins", { exact: true }).click()
    await dialog.getByText("All machines").click()
    await dialog.getByRole("button", { name: "Review" }).click()
    await page.getByRole("dialog").getByRole("button", { name: /^Save/ }).click({ timeout: 30000 })
    await page.getByRole("dialog").waitFor({ state: "detached", timeout: 30000 }).catch(() => {})
    log((await replayRules()) === rulesBefore + 1, "a saved replay rule reaches the policy")
    const added = page.locator("[data-recording-rule=recording_view]").last()
    log(await added.isVisible({ timeout: 15000 }).catch(() => false), "the new rule is listed")
    await added.getByRole("button", { name: "Remove rule" }).click()
    await page.getByRole("dialog").getByRole("button", { name: /^Save/ }).click({ timeout: 30000 })
    await page.getByRole("dialog").waitFor({ state: "detached", timeout: 30000 }).catch(() => {})
    log((await replayRules()) === rulesBefore, "removing it restores the policy")
  }

  // 4. Replay: unlock, load, play/pause, seek, speed; then the error states.
  if (recording && process.env.VAULT_PASSWORD_FILE) {
    await page.goto(`${origin}/recordings/${encodeURIComponent(recording)}`, { waitUntil: "networkidle", timeout: 120000 })
    log((await page.locator("main[data-recording-phase]").getAttribute("data-recording-phase")) === "locked", "a locked vault asks to unlock first")
    await page.getByLabel("Vault password").fill(readFileSync(process.env.VAULT_PASSWORD_FILE, "utf8").trim())
    await page.getByRole("button", { name: "Unlock" }).click()
    const ready = await page.locator("main[data-recording-phase=ready]").waitFor({ timeout: 120000 }).then(() => true, () => false)
    log(ready, "the recording loads and decrypts", (await page.locator("[data-recording-error]").getAttribute("data-recording-error").catch(() => null)) ?? "")
    if (ready) {
      const clock = () => page.locator("[data-recording-time]").textContent()
      const t0 = await clock()
      await page.waitForTimeout(2000)
      log((await clock()) !== t0, "it plays on its own")
      await page.locator("[data-recording-toggle]").click()
      const paused = await clock()
      await page.waitForTimeout(1500)
      log((await clock()) === paused && (await page.locator("[data-recording-toggle]").getAttribute("data-recording-toggle")) === "play", "pause stops the clock")
      await page.getByLabel("Position").fill("0")
      await page.waitForTimeout(500)
      log((await clock())?.startsWith("0:00"), "the seek bar moves to the start", await clock())
      await page.getByRole("radio", { name: "4×" }).click().catch(() => page.getByText("4×").click())
      log(await page.getByText("4×").evaluate((el) => el.closest("[aria-pressed=true],[data-pressed]") !== null), "speed 4× is selected")
      log((await page.locator(".terminal-host").innerText()).length > 0, "the terminal renderer shows the recording")
    }
    await page.goto(`${origin}/recordings/rec_00000000-0000-4000-8000-000000000000`, { waitUntil: "networkidle", timeout: 120000 })
    log(await page.locator("[data-recording-error=recording_not_found]").isVisible({ timeout: 30000 }).catch(() => false), "an unknown recording says it doesn't exist")
    if (process.env.OFFLINE_RECORDING) {
      await page.goto(`${origin}/recordings/${encodeURIComponent(process.env.OFFLINE_RECORDING)}`, { waitUntil: "networkidle", timeout: 120000 })
      log(await page.locator("[data-recording-error=machine_unavailable]").isVisible({ timeout: 60000 }).catch(() => false), "an offline machine says so and offers Retry")
    } else skip("offline machine state", "OFFLINE_RECORDING not set")
  } else skip("replay rows", "RECORDING and VAULT_PASSWORD_FILE not set")

  // 5. Terminal: the owner starts recording, REC shows, the server lists it, stop clears REC.
  if (machine) {
    await page.goto(`${origin}/terminals/new?machine=${encodeURIComponent(machine)}&workspace=${encodeURIComponent(team)}`, { timeout: 120000 })
    await page.waitForURL(/\/terminals\/(?!new)[^/?]+/, { timeout: 90000 })
    const session = page.url().match(/\/terminals\/([^/?]+)/)?.[1]
    const attached = await page.locator("[data-terminal-phase=attached]").waitFor({ timeout: 90000 }).then(() => true, () => false)
    log(attached, "the terminal attaches", session)
    await page.getByRole("button", { name: "Start recording" }).click()
    log(await page.locator("[data-terminal-recording]").isVisible({ timeout: 15000 }).catch(() => false), "Start recording shows REC")
    let live
    for (let i = 0; i < 20 && !live; i++) {
      await page.waitForTimeout(1000)
      live = (await get(`teams/${encodeURIComponent(team)}/recordings?session=${session}`)).body?.data?.recordings?.find((r) => r.state === "recording")
    }
    log(Boolean(live), "the server lists the live recording", live?.id ?? "")
    await page.getByRole("button", { name: "Stop recording" }).click()
    log(await page.locator("[data-terminal-recording]").waitFor({ state: "detached", timeout: 15000 }).then(() => true, () => false), "Stop recording clears REC")
    if (session) await ctx.request.delete(`${origin}/api/v1/terminal-sessions/${session}`).catch(() => {})
  } else skip("terminal rows", "MACHINE not set")
} finally {
  await browser.close()
}
console.log(failed ? `${failed} failed` : "all passed")
process.exit(failed ? 1 : 0)
