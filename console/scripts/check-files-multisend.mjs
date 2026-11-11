// Files page "Send files" to one or several machines at once. Signed-in console, live machines:
//   STATE=<playwright storage state> CHROME=<Chromium> CONSOLE_ORIGIN=<console origin> [PW_MODULE=<playwright-core index.mjs>]
//   [SHOTS=<dir for screenshots>] node check-files-multisend.mjs
// Uses the account's first two machines that can receive files (or MACHINES=<alias>,<alias>). Steps: (1) one machine: today's wording, no group
// headings, the file is in that machine's Inbox; (2) two machines: both chips, group headings, "Sent N files to 2
// machines' Inboxes.", the file is in both Inboxes; (3) the last chip has no remove button; (4) one machine refused
// (its shell start answered with the plan's browser-terminal limit in the browser, nothing real changes): the other
// still gets the file, the status reads "Sent to 1 of 2 machines. 1 failed." and keeps the error text.
// Files travel over the terminal's encrypted browser connection (a shell session per machine, closed afterwards). Sent test files are
// deleted from the Inboxes at the end. Prints PASS/FAIL per step; exits 1 on any FAIL.
import { mkdirSync } from "node:fs"

const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const { STATE, CHROME, CONSOLE_ORIGIN: origin, SHOTS: shots } = process.env
for (const [name, value] of Object.entries({ STATE, CHROME, CONSOLE_ORIGIN: origin }))
  if (!value) throw new Error(`${name} is not set; see the header of check-files-multisend.mjs`)
if (shots) mkdirSync(shots, { recursive: true })

const browser = await chromium.launch({ executablePath: CHROME })
const ctx = await browser.newContext({ storageState: STATE, viewport: { width: 1280, height: 900 } })
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const api = async (path) => {
  const res = await ctx.request.get(`${origin}/api/v1/${path}`)
  if (!res.ok()) throw new Error(`GET ${path} answered ${res.status()}; is STATE signed in?`)
  const body = await res.json()
  return body.data ?? body
}
const sent = []
const inboxHas = async (machine, name) => {
  const list = await api(`machines/${machine.id}/inbox`)
  return (list.items ?? []).find((item) => item.name === name)
}
const inboxHasWithin = async (machine, name, ms = 15000) => {
  for (const end = Date.now() + ms; Date.now() < end; await new Promise((r) => setTimeout(r, 1000)))
    if (await inboxHas(machine, name)) return true
  return false
}

const page = await ctx.newPage()
await page.goto(`${origin}/files`, { waitUntil: "networkidle", timeout: 120000 })
const workspace = (await page.evaluate(() => document.cookie.match(/(?:^|;\s*)pb-workspace=([a-z0-9-]+)/)?.[1])) ?? "personal"
const listing = await api(`machines?${new URLSearchParams({ workspace, limit: "200" })}`)
const usable = (listing.items ?? []).filter((m) => m.online && m.capabilities?.file_receive?.configured && m.capabilities?.file_receive?.observed)
if (usable.length < 2) throw new Error(`need two machines that can receive files, found ${usable.length}`)
const pick = process.env.MACHINES?.split(",").map((name) => usable.find((m) => m.alias === name))
const [a, b] = pick ?? usable
if (!a || !b) throw new Error(`MACHINES=${process.env.MACHINES} must name two machines that can receive files`)
const stamp = Date.now()
// "Send files" sits on a machine's Files page; the dialog starts with that machine.
await page.goto(`${origin}/files?machine=${a.id}`, { waitUntil: "networkidle", timeout: 120000 })
const dialog = page.getByRole("dialog")
const shot = (name) => (shots ? page.screenshot({ path: `${shots}/${name}.png` }) : undefined)
const openDialog = async (machines) => {
  await page.getByRole("button", { name: "Send files" }).first().click()
  await dialog.getByText("Drop files here").waitFor({ timeout: 30000 })
  // The default is one machine; add the others through "Add machine".
  for (const machine of machines.slice(1)) {
    await dialog.getByRole("button", { name: "Add machine" }).click()
    await page.getByRole("menuitemradio", { name: machine.alias, exact: true }).click()
  }
}
const chooseFile = (name) =>
  dialog.locator('input[type="file"]').setInputFiles({ name, mimeType: "text/plain", buffer: Buffer.from(`multisend ${stamp}\n`) })

// 1. one machine
await page.getByRole("button", { name: "Send files" }).first().click()
await dialog.getByText("Drop files here").waitFor({ timeout: 30000 })
const defaultChips = await dialog.locator("span.rounded-full").allInnerTexts()
log(defaultChips.length === 1, "the dialog starts with one machine chip", defaultChips.join(","))
log((await dialog.getByRole("button", { name: /^Remove / }).count()) === 0, "the only chip has no remove button")
const only = usable.find((m) => m.alias === defaultChips[0].trim()) ?? a
const one = `multisend-one-${stamp}.txt`
await chooseFile(one)
await dialog.getByText(new RegExp(`^Sent 1 of 1 to ${only.alias}’s Inbox\\.$`)).waitFor({ timeout: 120000 }).catch(() => {})
log(await dialog.getByText(`Sent 1 of 1 to ${only.alias}’s Inbox.`).isVisible(), "one machine keeps today's status wording")
log((await dialog.locator("h3").count()) === 0, "one machine shows no group heading")
log(await inboxHasWithin(only, one), `the file is in ${only.alias}'s Inbox`)
sent.push([only, one])
await dialog.getByRole("button", { name: "Done" }).click()

// 2. two machines
const two = `multisend-two-${stamp}.txt`
await openDialog([a, b])
log((await dialog.locator("span.rounded-full").count()) === 2, "adding a machine gives two chips")
log(await dialog.getByText("Files land in each machine’s Paperboat Inbox, in its Downloads folder.").isVisible(), "the help text says each machine")
await shot("desktop-picker")
await chooseFile(two)
await dialog.getByText("Sent 1 file to 2 machines’ Inboxes.").waitFor({ timeout: 120000 }).catch(() => {})
log(await dialog.getByText("Sent 1 file to 2 machines’ Inboxes.").isVisible(), "the status reads Sent 1 file to 2 machines' Inboxes.")
log((await dialog.locator("h3").allInnerTexts()).length === 2 && (await dialog.getByText("1 of 1 sent").count()) === 2, "each machine has a heading with its own count")
await shot("desktop-sent")
log(await inboxHasWithin(a, two), `the file is in ${a.alias}'s Inbox`)
log(await inboxHasWithin(b, two), `the file is in ${b.alias}'s Inbox`)
sent.push([a, two], [b, two])
await dialog.getByRole("button", { name: "Done" }).click()

// 3. removing a chip leaves the other, which then can't be removed
await openDialog([a, b])
await dialog.getByRole("button", { name: `Remove ${a.alias}` }).click()
log((await dialog.locator("span.rounded-full").count()) === 1 && (await dialog.getByRole("button", { name: /^Remove / }).count()) === 0, "removing one chip leaves one, without a remove button")
await page.keyboard.press("Escape")

// 4. one machine refused mid-send: the other still gets the file
const partial = `multisend-partial-${stamp}.txt`
const refuseB = new RegExp(`/api/v1/machines/${b.id}/terminal-sessions\\?`)
await page.route(refuseB, (route) =>
  route.request().method() === "POST"
    ? route.fulfill({ status: 403, contentType: "application/json", body: JSON.stringify({ error: { code: "browser_terminal_limit_reached", message: "Limit reached." } }) })
    : route.fallback(),
)
await openDialog([a, b])
await chooseFile(partial)
await dialog.getByText("Sent to 1 of 2 machines. 1 failed.").waitFor({ timeout: 120000 }).catch(() => {})
log(await dialog.getByText("Sent to 1 of 2 machines. 1 failed.").isVisible(), "a refused machine reads Sent to 1 of 2 machines. 1 failed.")
log(/browser terminal limit/.test(await dialog.locator("p.text-destructive").first().innerText().catch(() => "")), "the failed file keeps its error text (the plan limit, in words)", (await dialog.locator("p.text-destructive").first().innerText().catch(() => "")).slice(0, 80))
await shot("desktop-partial")
await page.unroute(refuseB)
log(await inboxHasWithin(a, partial), `${a.alias} still got the file`)
log(!(await inboxHas(b, partial)), `${b.alias} did not get it`)
sent.push([a, partial])
await dialog.getByRole("button", { name: "Done" }).click()

// Mobile and dark, for the screenshots.
if (shots) {
  await page.setViewportSize({ width: 375, height: 800 })
  await openDialog([a, b])
  await shot("mobile-picker")
  await chooseFile(`multisend-mobile-${stamp}.txt`)
  await dialog.getByText(/^Sent 1 file to 2 machines/).waitFor({ timeout: 120000 }).catch(() => {})
  await shot("mobile-sent")
  sent.push([a, `multisend-mobile-${stamp}.txt`], [b, `multisend-mobile-${stamp}.txt`])
  await dialog.getByRole("button", { name: "Done" }).click()
  await page.emulateMedia({ colorScheme: "dark" })
  await page.evaluate(() => document.documentElement.classList.add("dark"))
  await page.setViewportSize({ width: 1280, height: 900 })
  await openDialog([a, b])
  await shot("dark-picker")
  await page.keyboard.press("Escape")
}

// Clean up: delete the test files from the Inboxes.
for (const [machine, name] of sent) {
  const item = await inboxHas(machine, name)
  if (!item) continue
  const res = await ctx.request.delete(`${origin}/api/v1/machines/${machine.id}/inbox/${encodeURIComponent(item.id)}`, {
    headers: { "idempotency-key": crypto.randomUUID() }, // the console proxy adds the CSRF token
  })
  if (!res.ok() && res.status() !== 204) console.log(`note: could not delete ${name} from ${machine.alias} (${res.status()})`)
}
await browser.close()
console.log(failed ? `${failed} failed` : "all passed")
process.exit(failed ? 1 : 0)
