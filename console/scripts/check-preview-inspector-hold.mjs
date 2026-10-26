// Browser check: an inspector page left open for HOLD seconds (default 300, past several grant renewals) never reconnects:
// one ticket, phase Ready throughout, and a request sent at the end is listed.
// usage: ORIGIN=<console origin> STATE=<playwright storage state of the owner> HOLD=300 node check-preview-inspector-hold.mjs <previewId> <previewHost>
// ESM ignores NODE_PATH: PW_MODULE is the path of playwright-core's index.mjs when it is not installed next to this file.
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const [id, host] = process.argv.slice(2)
const origin = process.env.ORIGIN
const hold = Number(process.env.HOLD ?? 300)
const exe = process.env.PW_CHROMIUM ?? `${process.env.HOME}/.cache/ms-playwright/chromium_headless_shell-1248/chrome-headless-shell-linux-arm64/chrome-headless-shell`
const browser = await chromium.launch({ executablePath: exe })
const page = await (await browser.newContext({ storageState: process.env.STATE })).newPage()
let tickets = 0
let renewals = 0
page.on("response", (r) => {
  if (r.url().endsWith("/api/v1/inspector/browser-ticket")) tickets++
  if (r.url().endsWith("/renew") && r.status() === 200) renewals++
})
const results = []
const pass = (step, ok, detail = "") => { results.push(ok); console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`) }
await page.goto(`${origin}/previews/${id}/inspect`)
await page.waitForSelector("[data-inspector-phase=ready]", { timeout: 40000 })
await page.evaluate(() => {
  window.__ph = []
  setInterval(() => {
    const v = document.querySelector("[data-inspector-phase]")?.getAttribute("data-inspector-phase") ?? document.querySelector("[data-inspector-state]")?.getAttribute("data-inspector-state") ?? "none"
    if (window.__ph.at(-1) !== v) window.__ph.push(v)
  }, 100)
})
const capture = page.locator("label", { hasText: /^Capture$/ }).locator("[role=switch]")
if ((await capture.getAttribute("aria-checked")) !== "true") await capture.click()
await page.waitForTimeout(hold * 1000)
const phases = await page.evaluate(() => window.__ph)
pass(`page stayed Ready for ${hold} s with no reconnect`, phases.length === 1 && phases[0] === "ready", JSON.stringify(phases))
pass("one ticket only", tickets === 1, `${tickets} tickets`)
pass("grants were renewed in place meanwhile", hold < 200 || renewals > 0, `${renewals} renewals`)
const tag = `pbh-${Date.now()}`
await page.evaluate(([h, t]) => fetch(`https://${h}/${t}`, { mode: "no-cors", cache: "no-store" }).catch(() => {}), [host, tag])
const listed = await page.waitForFunction((t) => [...document.querySelectorAll("[data-capture-id]")].some((r) => r.innerText.includes(t)), tag, { timeout: 20000 }).then(() => true, () => false)
pass("a request after the hold is listed", listed)
await browser.close()
process.exit(results.every(Boolean) ? 0 : 1)
