// Runs a paste-in console check (console/scripts/check-*.js) in headless Chromium signed in with a stored session and
// prints PASS/FAIL per step; exits 1 when a step failed.
// usage: ORIGIN=<console origin> STATE=<playwright storage state of the owner> PW_CHROMIUM=<chromium path> [PW_MODULE=<playwright-core index.mjs>] \
//        node run-console-check.mjs <check.js> [page path] ['{"__global":"value"}']     
import { readFileSync } from "node:fs"

// ESM ignores NODE_PATH: PW_MODULE is the path of playwright-core's index.mjs when it is not installed next to this file.
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")

const origin = process.env.ORIGIN
const [script, path = "/", globals = "{}"] = process.argv.slice(2)
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
const page = await (await browser.newContext({ storageState: process.env.STATE })).newPage()
await page.goto(origin + path)
await page.waitForLoadState("networkidle").catch(() => {})
await page.waitForTimeout(4000)
await page.evaluate((g) => Object.assign(window, g), JSON.parse(globals))
const steps = await page.evaluate(readFileSync(script, "utf8"))
let failed = 0
for (const s of steps ?? []) {
  if (!s.ok) failed++
  console.log(`${s.ok ? "PASS" : "FAIL"}  ${s.step}${s.detail ? `  (${s.detail})` : ""}`)
}
console.log(`${(steps ?? []).length - failed}/${(steps ?? []).length} passed`)
await browser.close()
process.exit(failed || !steps?.length ? 1 : 0)
