// Console layout (W-14): Domains is a section of Network with each domain marked Private or Public, Recordings is a view of
// Terminals, the Environment page always shows the Vault section with the recovery path, the sidebar has no Domains or
// Recordings entry, and the old /domains and /recordings URLs redirect.
//   STATE=<playwright storage state> CHROME=<Chromium> CONSOLE_ORIGIN=<console origin> [PW_MODULE=<playwright-core index.mjs>]
//   node check-layout.mjs
// Read-only: nothing is created or changed. The vault stays locked, so the Vault section shows its closed state.
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const { STATE, CHROME, CONSOLE_ORIGIN: origin } = process.env
for (const [name, value] of Object.entries({ STATE, CHROME, CONSOLE_ORIGIN: origin }))
  if (!value) throw new Error(`${name} is not set; see the header of check-layout.mjs`)
const browser = await chromium.launch({ executablePath: CHROME })
const ctx = await browser.newContext({ storageState: STATE, viewport: { width: 1280, height: 900 } })
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const page = await ctx.newPage()
const open = (path) => page.goto(origin + path, { waitUntil: "networkidle", timeout: 120000 })
const path = () => new URL(page.url()).pathname + new URL(page.url()).search + new URL(page.url()).hash

await open("/network")
const nav = page.locator('[data-slot="sidebar"] a, nav a')
const hrefs = await nav.evaluateAll((els) => els.map((a) => new URL(a.href).pathname))
log(hrefs.includes("/network"), "the sidebar has Network")
log(!hrefs.includes("/domains") && !hrefs.includes("/recordings"), "the sidebar has no Domains or Recordings entry")

const domains = page.locator("section#domains")
await domains.waitFor({ timeout: 30000 }).catch(() => {})
log((await domains.count()) === 1, "Network has a Domains section")
const text = await domains.innerText().catch(() => "")
log(/Private/.test(text) && /Public/.test(text), "it groups Private and Public domains")
const marks = await domains.locator('[data-slot="badge"]').allInnerTexts().catch(() => [])
log(marks.length > 0 && marks.every((m) => m === "Private" || m === "Public"), "every row is marked Private or Public", marks.join(","))
log(marks.includes("Private"), "the machine domain is marked Private")
const heading = domains.locator("h2, h3").filter({ hasText: /Domains/ }).first()
log((await heading.count()) === 1, "the section is headed Domains")

await open("/domains")
log(path() === "/network#domains", "/domains redirects to Network, Domains section", path())
await page.waitForTimeout(1500)
log((await page.locator("section#domains").count()) === 1, "the redirect lands on the Domains section")

await open("/terminals")
const recordings = page.getByRole("radio", { name: "Recordings" }).or(page.getByRole("tab", { name: "Recordings" })).or(page.getByRole("button", { name: /^Recordings/ }))
log((await recordings.count()) >= 1, "Terminals has a Recordings view next to Open and Closed")
await recordings.first().click()
await page.waitForTimeout(1500)
const pressed = () => recordings.first().getAttribute("aria-pressed")
log((await pressed()) === "true" && path().startsWith("/terminals"), "choosing it shows recordings inside Terminals", path())

await open("/recordings")
log(path() === "/terminals?view=recordings", "/recordings redirects to Terminals?view=recordings", path())
await page.waitForTimeout(1500)
log((await pressed()) === "true", "the redirect opens the recordings view")

await open("/environment")
const vault = page.getByText("Vault", { exact: true }).first()
await vault.waitFor({ timeout: 30000 }).catch(() => {})
log((await vault.count()) === 1, "Environment shows the Vault section")
const env = await page.locator('[data-slot="sidebar-inset"]').innerText()
log(/Not set up|Locked|Password/.test(env), "the Vault section says its state")
log(/recovery code|Forgot password/i.test(env) || /Not set up/.test(env), "the recovery path is in sight (or the vault is not set up yet)")

await browser.close()
console.log(failed ? `${failed} failed` : "all passed")
process.exit(failed ? 1 : 0)
