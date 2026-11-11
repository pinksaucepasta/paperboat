// Teams session recordings in the console: settings, list, replay and the terminal's REC control.
//   STATE=<storage state of a team owner> CHROME=<Chromium> CONSOLE_ORIGIN=<console origin> TEAM=<team workspace id>
//   [MACHINE=<team machine id, online, with a recording key set up>]  terminal rows: start, REC, stop
//   [RECORDING=<recording id> VAULT_PASSWORD_FILE=<file holding the owner's vault password>]  replay rows
//   [OFFLINE_RECORDING=<recording id on an offline machine>]  the offline state
//   [LONG_RECORDING=<id of a recording of tens of MB> VAULT_PASSWORD_FILE]  removing replay permission closes an open replay.
//     DESTRUCTIVE: removing the team's only replay rule deletes the owner's sealed copy of the recording key (by design), and
//     adding the rule back does not restore it. Run it last, on a team whose recording key you can set up again.
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
  log(await page.locator("[data-recordings-upgrade]").waitFor({ timeout: 20000 }).then(() => true, () => false), "Personal shows the Teams upgrade state")

  // 2. The team's list matches the server's first page.
  await workspace(team)
  const list = await get(`teams/${encodeURIComponent(team)}/recordings?limit=50&offset=0`)
  await page.goto(`${origin}/recordings`, { waitUntil: "networkidle", timeout: 120000 })
  if (list.status === 409) log(await page.locator("[data-recordings-upgrade]").isVisible(), "a team without the plan shows the upgrade state")
  else {
    const rows = list.body?.data?.recordings ?? []
    if (rows.length === 0) log(await page.getByText("No recordings yet").waitFor({ timeout: 20000 }).then(() => true, () => false), "an empty team shows No recordings yet")
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
  if (key.status === 409) log(await page.locator("[data-recordings-upgrade]").waitFor({ timeout: 20000 }).then(() => true, () => false), "settings show the upgrade state without the plan")
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
    log(await dialog.getByText("Record sessions").waitFor({ timeout: 5000 }).then(() => true, () => false), "Add rule opens its dialog")
    await dialog.getByText("Allow replay").click()
    await dialog.getByText("Admins", { exact: true }).click()
    await dialog.getByText("All machines").click()
    await dialog.getByRole("button", { name: "Review" }).click()
    await page.getByRole("dialog").getByRole("button", { name: /^Save/ }).click({ timeout: 30000 })
    await page.getByRole("dialog").waitFor({ state: "detached", timeout: 30000 }).catch(() => {})
    log((await replayRules()) === rulesBefore + 1, "a saved replay rule reaches the policy")
    const added = page.locator("[data-recording-rule=recording_view]").last()
    log(await added.waitFor({ timeout: 15000 }).then(() => true, () => false), "the new rule is listed")
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
    log(ready, "the recording loads and decrypts", (ready ? "" : await page.locator("[data-recording-error]").first().getAttribute("data-recording-error", { timeout: 2000 }).catch(() => "")))
    if (ready) {
      const clock = () => page.locator("[data-recording-time]").textContent()
      const until = async (probe, ms = 6000) => {
        for (let waited = 0; waited < ms; waited += 250) {
          if (await probe()) return true
          await page.waitForTimeout(250)
        }
        return false
      }
      const t0 = await clock()
      log(await until(async () => (await clock()) !== t0), "it plays on its own", `${t0} -> ${await clock()}`)
      await page.locator("[data-recording-toggle]").click()
      const paused = await clock()
      await page.waitForTimeout(1500)
      log((await clock()) === paused && (await page.locator("[data-recording-toggle]").getAttribute("data-recording-toggle")) === "play", "pause stops the clock", paused)
      log(await until(async () => (await page.locator(".terminal-host").innerHTML()).length > 0), "the terminal renderer shows the recording")
      await page.getByRole("button", { name: "4×" }).click()
      log((await page.getByRole("button", { name: "4×" }).getAttribute("aria-pressed")) === "true", "speed 4× is selected")
      // A keyboard seek to the start (an empty screen is right there: the first output comes after it).
      await page.getByLabel("Position").press("Home")
      log(await until(async () => (await clock())?.startsWith("0:00")), "the seek bar moves to the start", await clock())
    }
    // A full navigation locks the vault again: unlock before the error states, which sit behind it.
    const unlock = async () => {
      if ((await page.locator("main[data-recording-phase]").getAttribute("data-recording-phase")) !== "locked") return
      await page.getByLabel("Vault password").fill(readFileSync(process.env.VAULT_PASSWORD_FILE, "utf8").trim())
      await page.getByRole("button", { name: "Unlock" }).click()
    }
    await page.goto(`${origin}/recordings/rec_00000000-0000-4000-8000-000000000000`, { waitUntil: "networkidle", timeout: 120000 })
    await unlock()
    log(await page.locator("[data-recording-error=recording_not_found]").waitFor({ timeout: 30000 }).then(() => true, () => false), "an unknown recording says it doesn't exist")
    if (process.env.OFFLINE_RECORDING) {
      await page.goto(`${origin}/recordings/${encodeURIComponent(process.env.OFFLINE_RECORDING)}`, { waitUntil: "networkidle", timeout: 120000 })
      await unlock()
      log(await page.locator("[data-recording-error=machine_unavailable]").waitFor({ timeout: 60000 }).then(() => true, () => false), "an offline machine says so and offers Retry")
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
    log(await page.locator("[data-terminal-recording]").waitFor({ timeout: 15000 }).then(() => true, () => false), "Start recording shows REC")
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

  // 6. Removing the viewer's replay permission closes a replay that is still downloading (F25-05). The team's replay
  // rules are removed in the settings while the replay streams, then added back.
  if (process.env.LONG_RECORDING && process.env.VAULT_PASSWORD_FILE) {
    const settings = await ctx.newPage()
    await settings.goto(`${origin}/team?team=${encodeURIComponent(team)}&tab=recordings`, { waitUntil: "networkidle", timeout: 120000 })
    await settings.locator("[data-recording-settings]").waitFor({ timeout: 30000 })
    const saveDialog = async () => {
      await settings.getByRole("dialog").getByRole("button", { name: /^Save/ }).click({ timeout: 30000 })
      await settings.getByRole("dialog").waitFor({ state: "detached", timeout: 30000 }).catch(() => {})
    }
    const ruleCount = () => settings.locator("[data-recording-rule=recording_view]").count()
    const removed = await ruleCount()
    const viewer = await ctx.newPage()
    await viewer.goto(`${origin}/recordings/${encodeURIComponent(process.env.LONG_RECORDING)}`, { waitUntil: "networkidle", timeout: 120000 })
    await viewer.getByLabel("Vault password").fill(readFileSync(process.env.VAULT_PASSWORD_FILE, "utf8").trim())
    await viewer.getByRole("button", { name: "Unlock" }).click()
    const downloading = await viewer.locator("[data-recording-loading]").waitFor({ timeout: 60000 }).then(() => true, () => false)
    log(downloading, "the long recording is downloading")
    for (let i = 0; i < removed; i++) {
      await settings.locator("[data-recording-rule=recording_view]").first().getByRole("button", { name: "Remove rule" }).click()
      await saveDialog()
    }
    const t0 = Date.now()
    const outcome = await viewer.locator("main[data-recording-phase]").evaluate(async (main) => {
      for (let i = 0; i < 240; i++) {
        const phase = main.getAttribute("data-recording-phase")
        if (phase === "failed" || phase === "ready") return phase
        await new Promise((r) => setTimeout(r, 500))
      }
      return "loading"
    }).catch(() => "unknown")
    if (outcome === "ready") skip("permission removal closes the replay", "the whole recording had downloaded before the rule was removed; use a longer recording")
    else log(outcome === "failed", "removing replay permission ends the open replay", `${outcome} after ${Math.round((Date.now() - t0) / 1000)} s`)
    // Put the rules back: one for admins on every machine, as the team had.
    if (removed > 0 && (await ruleCount()) === 0) {
      await settings.getByRole("button", { name: "Add rule" }).click()
      const dialog = settings.getByRole("dialog")
      await dialog.getByText("Allow replay").click()
      await dialog.getByText("Admins", { exact: true }).click()
      await dialog.getByText("All machines").click()
      await dialog.getByRole("button", { name: "Review" }).click()
      await saveDialog()
    }
    log((await ruleCount()) >= removed, "the replay rules are back", `${await ruleCount()}`)
    await viewer.close()
    await settings.close()
  } else skip("permission removal closes the replay", "LONG_RECORDING and VAULT_PASSWORD_FILE not set")
} finally {
  await browser.close()
}
console.log(failed ? `${failed} failed` : "all passed")
process.exit(failed ? 1 : 0)
