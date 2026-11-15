// Browser check for the header's Requests: the bell shows only while something waits, counts come from the servers'
// totals, each list pages 10 at a time, and Accept/Decline answer inline. The three sources are stubbed in the tab
// (13 file requests, 1 invitation, 2 machine requests of a stubbed team the account administers); nothing on the
// server changes.
//   STATE=<storage state of any signed-in account> CHROME=<Chromium> CONSOLE_ORIGIN=<console origin> node check-requests-menu.mjs
import { chromium } from "playwright-core"

const { STATE, CHROME, CONSOLE_ORIGIN: origin } = process.env
for (const [name, value] of Object.entries({ STATE, CHROME, CONSOLE_ORIGIN: origin }))
  if (!value) throw new Error(`${name} is not set; see the header of check-requests-menu.mjs`)

let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const json = (route, data) => route.fulfill({ contentType: "application/json", body: JSON.stringify({ data }) })

const state = { files: 13, invitations: 1, machines: ["m-one", "m-two"], answers: [] }
const fileRequest = (index) => ({
  request_id: `req-${index}`, team_id: "stubteam", sender_account: "acct-other", recipient_account: "me",
  source_machine_id: "src", source_machine_name: "laptop", destination_machine_id: "dst", destination_machine_name: "build box",
  files: [{ basename: `file-${index}.txt`, size: 2048, sha256: "x" }], status: "pending", decision_generation: 1,
  expires_at: new Date(Date.now() + 86_400_000).toISOString(),
})
const invitation = () => ({
  invitation_id: "inv-1", team_id: "stubteam", account_id: "me", role: "member", state: "pending", created_by: "acct-other",
  email: "me@example.test", created_by_email: "owner@example.test", expires_at: new Date(Date.now() + 86_400_000).toISOString(),
})
const paged = (rows, params) => {
  const offset = Number(params.get("offset") ?? 0), limit = Number(params.get("limit") ?? 50)
  return { rows: rows.slice(offset, offset + limit), pagination: { limit, offset, total: rows.length } }
}

const browser = await chromium.launch({ executablePath: CHROME })
const ctx = await browser.newContext({ storageState: STATE, viewport: { width: 1280, height: 900 } })
const page = await ctx.newPage()
await ctx.route("**/api/v1/**", async (route) => {
  const url = new URL(route.request().url())
  const path = url.pathname.replace("/api/v1/", "")
  const method = route.request().method()
  if (path === "team-inbox/requests" && method === "GET") {
    const all = Array.from({ length: state.files }, (_, i) => fileRequest(i + 1))
    const { rows, pagination } = paged(all, url.searchParams)
    return json(route, { requests: rows, pagination })
  }
  const decide = path.match(/^team-inbox\/requests\/(req-\d+)\/(approve|decline)$/)
  if (decide && method === "POST") {
    state.files--, state.answers.push(`${decide[1]} ${decide[2]}`)
    return json(route, {})
  }
  if (path === "team-invitations" && method === "GET") {
    const { rows, pagination } = paged(Array.from({ length: state.invitations }, invitation), url.searchParams)
    return json(route, { items: rows, pagination })
  }
  const answer = path.match(/^team-invitations\/inv-1\/(accept|decline)$/)
  if (answer && method === "POST") {
    state.invitations--, state.answers.push(`inv-1 ${answer[1]}`)
    return json(route, {})
  }
  if (path === "workspaces" && method === "GET") {
    const real = await (await route.fetch()).json()
    real.data.items = [...real.data.items.filter((w) => w.id !== "stubteam"), { id: "stubteam", name: "stubteam", kind: "team", role: "admin" }]
    return route.fulfill({ contentType: "application/json", body: JSON.stringify(real) })
  }
  if (path === "teams/stubteam" && method === "GET")
    return json(route, {
      team_id: "stubteam", display_name: "Stub team", generation: 1, role: "admin",
      machine_requests: state.machines.map((id) => ({ machine_id: id, alias: id, requested_by: "acct-other", state: "pending", requested_at: new Date().toISOString() })),
    })
  if (path === "teams/stubteam/machines" && method === "POST") {
    const body = route.request().postDataJSON()
    state.machines = state.machines.filter((id) => id !== body.machine_id)
    state.answers.push(`${body.machine_id} ${body.action}`)
    return json(route, { team_id: "stubteam", generation: 2 })
  }
  return route.continue()
})

const bell = page.getByRole("button", { name: /^Requests, \d+ waiting$/ })
const refresh = async () => page.evaluate(() => document.dispatchEvent(new Event("visibilitychange")))
const waitBell = async (text) => bell.filter({ hasText: text }).waitFor({ timeout: 20000 }).then(() => true, () => false)

state.files = 0, state.invitations = 0, state.machines = []
await page.goto(`${origin}/`, { waitUntil: "networkidle" })
log((await bell.count()) === 0, "no bell while nothing waits")

// Reload with things waiting.
state.files = 13, state.invitations = 1, state.machines = ["m-one", "m-two"]
await page.goto(`${origin}/`, { waitUntil: "networkidle" })
log(await waitBell("16"), "bell counts every source's total (13 + 1 + 2)")

await bell.click()
const tab = (name) => page.getByRole("tab", { name })
log(
  (await tab(/^Files 13$/).count()) === 1 && (await tab(/^Invitations 1$/).count()) === 1 && (await tab(/^Machines 2$/).count()) === 1,
  "one tab per kind with its own count",
)
const rows = () => page.locator("[data-slot=popover-content]").getByRole("button", { name: "Accept" }).count()
log((await rows()) === 10, "Files page 1 lists 10 requests", String(await rows()))
log(await page.getByText("1–10 of 13").isVisible(), "pager reads 1–10 of 13")
await page.getByRole("button", { name: "Next" }).click()
await page.getByText("11–13 of 13").waitFor({ timeout: 10000 }).catch(() => {})
log((await rows()) === 3 && (await page.getByText("file-11.txt").isVisible()), "Next shows requests 11–13")

await page.getByRole("button", { name: "Accept" }).first().click()
await page.getByText("Accepted").waitFor({ timeout: 10000 }).catch(() => {})
log(state.answers.at(-1) === "req-11 approve", "Accept approves that request", state.answers.at(-1))
log(await waitBell("15"), "count drops to 15 after the answer")

await tab(/^Invitations 1$/).click()
log(await page.getByText("Join stubteam as member").isVisible(), "invitation reads 'Join stubteam as member'")
await page.getByRole("button", { name: "Decline" }).first().click()
await page.waitForFunction(() => !document.body.innerText.includes("Invitations 1"), null, { timeout: 15000 }).catch(() => {})
log(state.answers.at(-1) === "inv-1 decline", "Decline declines the invitation", state.answers.at(-1))

await tab(/^Machines 2$/).click()
log(await page.getByText("m-one").first().isVisible(), "machine request names the machine and team")
await page.getByRole("button", { name: "Accept" }).first().click()
await page.waitForFunction(() => !document.body.innerText.includes("Machines 2"), null, { timeout: 15000 }).catch(() => {})
log(state.answers.at(-1) === "m-one approve", "Accept approves the machine", state.answers.at(-1))

// Answer the rest: the bell goes away when nothing waits.
state.files = 0, state.machines = []
await page.keyboard.press("Escape")
await refresh()
await page.reload({ waitUntil: "networkidle" })
log((await bell.count()) === 0, "bell is gone again when nothing waits")

await browser.close()
process.exit(failed ? 1 : 0)
