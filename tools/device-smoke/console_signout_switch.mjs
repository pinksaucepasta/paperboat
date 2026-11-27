// F2-07: workspace switching and sign-out in the console.
//   run: node console_signout_switch.mjs run TEAM
//   end: node console_signout_switch.mjs end            signs out STATE's session if it still lives (cleanup)
// env: ORIGIN, STATE (a session made for THIS run: it is signed out), OPERATOR_STATE (the shared session, only read:
// `run` proves it still works afterwards), PW_CHROMIUM, [PW_MODULE]. It refuses to sign out the shared session.
// Nothing secret is printed.
import { readFileSync, realpathSync } from "node:fs"

const origin = process.env.ORIGIN
const [action, team] = process.argv.slice(2)
if (!(action === "run" && team) && action !== "end") {
  console.error("usage: console_signout_switch.mjs run TEAM | end")
  process.exit(2)
}
const cookies = (file) => JSON.parse(readFileSync(file, "utf8")).cookies.filter((c) => /pb-(dev-)?session$/.test(c.name)).map((c) => c.value)
const shared = process.env.OPERATOR_STATE
if (!process.env.STATE || !shared || realpathSync(process.env.STATE) === realpathSync(shared) || cookies(process.env.STATE).some((v) => cookies(shared).includes(v))) {
  console.error("refused: STATE must be a session of its own, not the shared OPERATOR_STATE")
  process.exit(2)
}
// Imported after the refusal: it needs no browser, and nothing may touch the shared session.
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
  return ok
}
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
const until = async (probe, ms = 30000) => {
  for (const end = Date.now() + ms; Date.now() < end; await sleep(300)) {
    const found = await probe().catch(() => false)
    if (found) return found
  }
  return false
}

const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
const open = async (state) => browser.newContext({ storageState: state, viewport: { width: 1280, height: 900 } })
const me = async (context) => (await context.request.get(`${origin}/api/v1/me`)).status()
const context = await open(process.env.STATE)
const page = await context.newPage()

async function signOut() {
  await page.goto(`${origin}/`, { waitUntil: "networkidle", timeout: 120000 }).catch(() => {})
  await page.locator('[data-slot="sidebar-footer"] button').first().click({ timeout: 60000 })
  await page.getByRole("menuitem", { name: "Sign out" }).click({ timeout: 15000 })
}

async function run() {
  log((await me(context)) === 200, "the fresh session is signed in")
  const listed = (await (await context.request.get(`${origin}/api/v1/workspaces`)).json()).data?.items ?? []
  const wanted = listed.find((w) => w.id === team)
  const personal = listed.find((w) => w.kind === "personal")
  if (!log(Boolean(wanted && personal), "the account has Personal and the team", `${listed.length} workspaces`)) return

  await page.goto(`${origin}/`, { waitUntil: "networkidle", timeout: 120000 }).catch(() => {})
  const crumb = (name) => page.locator("button[aria-haspopup=dialog]").filter({ hasText: name }).first()
  const cookie = async () => (await context.cookies(origin)).find((c) => c.name === "pb-workspace")?.value ?? "personal"
  const card = () =>
    page.evaluate(() => {
      const el = [...document.querySelectorAll("section > div")].find((c) => /^Machines/.test(c.innerText.trim()) && c.querySelector("[data-machine-platforms]"))
      const n = el?.innerText.match(/(\d+)\s*Online[\s\S]*?(\d+)\s*Offline/)
      return n ? `${n[1]}/${n[2]}` : ""
    })
  const summary = async (id) => {
    const data = (await (await context.request.get(`${origin}/api/v1/machines/summary?workspace=${encodeURIComponent(id)}`)).json()).data
    return `${data.by_state.online}/${data.total - data.by_state.online}`
  }
  const shows = async (workspace, label) => {
    const expected = await summary(workspace.id)
    // A workspace without machines draws no card.
    const ok = await until(async () => {
      const got = await card()
      return got === expected || (expected === "0/0" && got === "")
    })
    log(Boolean(ok), `${label}: Home counts the machines of ${workspace.kind === "team" ? "the team" : "Personal"}`, `page ${(await card()) || "no card"}, server ${expected}`)
  }
  const choose = async (from, to) => {
    await crumb(from.name).click({ timeout: 60000 })
    await page.getByRole("option").filter({ hasText: to.name }).first().click({ timeout: 15000 })
    const ok = await until(async () => (await cookie()) === to.id && (await crumb(to.name).count()) > 0)
    return log(Boolean(ok), `the switcher changes ${from.kind} → ${to.kind}`, `workspace ${await cookie()}`)
  }
  await shows(personal, "at the start")
  if (await choose(personal, wanted)) {
    await shows(wanted, "after switching to the team")
    await page.reload({ waitUntil: "networkidle", timeout: 120000 }).catch(() => {})
    log(Boolean(await until(async () => (await crumb(wanted.name).count()) > 0)), "a reload keeps the team selected")
    await page.goto(`${origin}/team`, { waitUntil: "networkidle", timeout: 120000 }).catch(() => {})
    log(Boolean(await until(async () => (await page.getByRole("button", { name: /Invite members/ }).count()) > 0)), "Team shows the selected team's members page")
    await page.goto(`${origin}/`, { waitUntil: "networkidle", timeout: 120000 }).catch(() => {})
    if (await choose(wanted, personal)) {
      await shows(personal, "back in Personal")
      await page.goto(`${origin}/team`, { waitUntil: "networkidle", timeout: 120000 }).catch(() => {})
      log(Boolean(await until(async () => (await page.getByRole("button", { name: "Create team" }).count()) > 0)), "Team in Personal shows the teams list")
    }
  }

  const before = await context.storageState()
  await signOut()
  const out = await until(async () => (await me(context)) === 401, 90000)
  log(Boolean(out), "Sign out ends the browser's session", `/me ${await me(context)}`)
  await page.waitForLoadState("domcontentloaded").catch(() => {})
  await sleep(5000)
  await page.goto(`${origin}/machines`, { waitUntil: "networkidle", timeout: 120000 }).catch(() => {})
  const signedOut = () => !(new URL(page.url()).host === new URL(origin).host && new URL(page.url()).pathname === "/machines")
  log(signedOut(), "after sign-out /machines goes to sign-in", new URL(page.url()).host)
  // The server revoked it, not only the browser's cookies: the cookies saved before the sign-out are refused.
  const replay = await open(before)
  const status = await me(replay)
  log(status === 401, "the signed-out session's old cookies are refused", `HTTP ${status}`)
  const owner = await open(shared)
  log((await me(owner)) === 200, "the other session of the same account (the shared operator session) still works")
}

try {
  if (action === "run") await run()
  else if ((await me(context)) === 200) {
    await signOut()
    if (!(await until(async () => (await me(context)) === 401, 90000))) throw new Error("the run's session is still signed in")
    console.log("the run's session was signed out")
  }
} catch (error) {
  failed++
  console.log(`FAIL  ${action} stopped  (${String(error.message).split("\n")[0].slice(0, 200)})`)
} finally {
  await browser.close()
}
process.exit(failed ? 1 : 0)
