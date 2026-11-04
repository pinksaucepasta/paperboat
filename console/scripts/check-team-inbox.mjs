// F6-06: a team owner sees pending Team Inbox requests on the console Files page and approves or declines them there.
// Fixture (the caller makes it first, no devices needed here): a teammate runs `pb send` of two files with distinct
// names to the owner's team machine, so two requests are pending; APPROVE_FILE and DECLINE_FILE are those names
// (the owner needs no machine online for the UI part; delivery of the approved bytes is the CLI driver's check).
//   STATE=<owner's storage state> CHROME=<Chromium> CONSOLE_ORIGIN=<console origin> TEAM=<team workspace slug>
//   APPROVE_FILE=<file name to approve> DECLINE_FILE=<file name to decline> node check-team-inbox.mjs
// Steps: both requests are pending in GET /v1/team-inbox/requests and listed under "Waiting for you"; Approve on one
// and Decline on the other each remove their row; the API then says approved (or consumed/completed: the server's approved -> consumed -> completed lifecycle) and declined; the
// other request is untouched until its own button is pressed. File names and tokens are the only things printed.
import { chromium } from "playwright-core"

const { STATE, CHROME, CONSOLE_ORIGIN: origin, TEAM: team, APPROVE_FILE: approveFile, DECLINE_FILE: declineFile } = process.env
for (const [name, value] of Object.entries({ STATE, CHROME, CONSOLE_ORIGIN: origin, TEAM: team, APPROVE_FILE: approveFile, DECLINE_FILE: declineFile }))
  if (!value) throw new Error(`${name} is not set; see the header of check-team-inbox.mjs`)
if (approveFile === declineFile) throw new Error("APPROVE_FILE and DECLINE_FILE must be different files")

const browser = await chromium.launch({ executablePath: CHROME })
const ctx = await browser.newContext({ storageState: STATE, viewport: { width: 1280, height: 900 } })
await ctx.addCookies([{ name: "pb-workspace", value: team, url: origin }])
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const requests = async () => {
  const res = await ctx.request.get(`${origin}/api/v1/team-inbox/requests?workspace=${encodeURIComponent(team)}`)
  if (!res.ok()) throw new Error(`GET team-inbox/requests answered ${res.status()}; is STATE signed in as a member of ${team}?`)
  const body = await res.json()
  return (body.data ?? body).requests ?? []
}
const named = (list, name) => list.filter((r) => r.files.some((f) => f.basename === name))
const statusOf = async (name) => named(await requests(), name).map((r) => r.status)

const before = await requests()
log(named(before, approveFile).some((r) => r.status === "pending"), "the API lists the request to approve as pending", approveFile)
log(named(before, declineFile).some((r) => r.status === "pending"), "the API lists the request to decline as pending", declineFile)

const page = await ctx.newPage()
await page.goto(`${origin}/files`, { waitUntil: "networkidle", timeout: 120000 })
const waiting = page.locator("div.py-3", { has: page.getByRole("button", { name: "Approve" }) })
const rowOf = (name) => waiting.filter({ hasText: name }).first()
try {
  await page.getByText("Waiting for you").first().waitFor({ timeout: 45000 })
} catch {
  log(false, "Files shows the 'Waiting for you' section", `not shown for ${team}; is STATE the team owner or an admin with a machine named in the requests?`)
}
log((await rowOf(approveFile).count()) === 1 && (await rowOf(declineFile).count()) === 1, "both requests are listed under Waiting for you")
if (failed) {
  await browser.close()
  console.log(`${failed} failed before any click: create the fixture first (teammate \`pb send\` of both files to the owner's machine)`)
  process.exit(1)
}
const text = (await rowOf(approveFile).innerText().catch(() => "")).replace(/\s+/g, " ")
log(/A teammate wants to send .* to .* expires/.test(text), "a row names the size, destination machine and expiry", text.slice(0, 120))

await rowOf(approveFile).getByRole("button", { name: "Approve" }).click()
await rowOf(approveFile).waitFor({ state: "detached", timeout: 30000 }).catch(() => {})
log((await rowOf(approveFile).count()) === 0, "Approve removes its row")
log((await rowOf(declineFile).count()) === 1, "the other request still waits")
log(await page.getByText(/Approved; it arrives in/).first().isVisible().catch(() => false), "a toast confirms the approval")

await rowOf(declineFile).getByRole("button", { name: "Decline" }).click()
await rowOf(declineFile).waitFor({ state: "detached", timeout: 30000 }).catch(() => {})
log((await rowOf(declineFile).count()) === 0, "Decline removes its row")

const approved = await statusOf(approveFile)
const declined = await statusOf(declineFile)
log(approved.length > 0 && approved.every((s) => ["approved", "consumed", "completed"].includes(s)), "the API says approved", approved.join(","))
log(declined.length > 0 && declined.every((s) => s === "declined"), "the API says declined", declined.join(","))

await page.reload({ waitUntil: "networkidle" })
log((await rowOf(approveFile).count()) + (await rowOf(declineFile).count()) === 0, "after a reload neither request waits any more")
await page.screenshot({ path: "/tmp/team-inbox.png" })
await browser.close()
console.log(failed ? `${failed} failed` : "all passed")
process.exit(failed ? 1 : 0)
