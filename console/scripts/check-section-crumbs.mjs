// The section crumb of "Section > machine" breadcrumbs is a link back to the section's list while a machine is chosen
// (Terminals, Files, Previews, Tunnels, Environment, Config, Usage, Activity), plain text on the list itself, focusable
// from the keyboard; Files lands on the Choose machine page. Machine pages link "Machines" back to the list.
//   STATE=<playwright storage state> CHROME=<Chromium> CONSOLE_ORIGIN=<console origin> [PW_MODULE=<playwright-core index.mjs>]
//   [SHOTS=<dir for a screenshot>] node check-section-crumbs.mjs
// Uses any online machine of the signed-in account; nothing is changed.
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const { STATE, CHROME, CONSOLE_ORIGIN: origin, SHOTS: shots } = process.env
for (const [name, value] of Object.entries({ STATE, CHROME, CONSOLE_ORIGIN: origin }))
  if (!value) throw new Error(`${name} is not set; see the header of check-section-crumbs.mjs`)
const browser = await chromium.launch({ executablePath: CHROME })
const ctx = await browser.newContext({ storageState: STATE, viewport: { width: 1280, height: 800 } })
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const page = await ctx.newPage()
await page.goto(`${origin}/files`, { waitUntil: "networkidle", timeout: 120000 })
const res = await ctx.request.get(`${origin}/api/v1/machines?workspace=personal&limit=200`)
const machine = ((await res.json()).data ?? {}).items?.find((m) => m.online)
if (!machine) throw new Error("need an online machine")
const crumbs = page.locator('nav[aria-label="breadcrumb"]')
const sections = ["/terminals", "/files", "/previews", "/tunnels", "/environment", "/config", "/usage", "/activity"]
for (const href of sections) {
  await page.goto(`${origin}${href}?machine=${machine.id}`, { waitUntil: "networkidle", timeout: 120000 })
  const link = crumbs.locator(`a[href="${href}"]`)
  await link.waitFor({ timeout: 20000 }).catch(() => {})
  log((await link.count()) === 1, `${href}: the section crumb is a link while a machine is chosen`)
  if (!(await link.count())) continue
  await link.focus()
  log(await link.evaluate((el) => el === document.activeElement), `${href}: it takes keyboard focus`)
  if (href === "/files" && shots) await page.screenshot({ path: `${shots}/crumb-files-focus.png` })
  await (href === "/files" ? link.click() : page.keyboard.press("Enter"))
  await page.waitForURL((url) => url.pathname === href && !url.searchParams.has("machine"), { timeout: 20000 }).catch(() => {})
  const url = new URL(page.url())
  log(url.pathname === href && !url.searchParams.has("machine"), `${href}: it leads back to the list`, `${url.pathname}${url.search}`)
  const text = await crumbs.innerText()
  log(href === "/files" ? text.includes("Choose machine") : text.includes("All machines"), `${href}: the machine crumb reads ${href === "/files" ? "Choose machine" : "All machines"}`)
  if (href === "/files") log(await page.getByPlaceholder("Search machines").isVisible(), "/files: the Choose machine list is shown")
  log((await crumbs.locator(`a[href="${href}"]`).count()) === 0, `${href}: on the list the section crumb is plain text`)
}
await page.goto(`${origin}/machines/${machine.id}`, { waitUntil: "networkidle", timeout: 120000 })
log((await crumbs.locator('a[href="/"]').count()) === 1, "a machine page links Machines back to the list")
await browser.close()
console.log(failed ? `${failed} failed` : "all passed")
process.exit(failed ? 1 : 0)
