// check-all-pages.js returns its findings (an empty list when every page is clean), which run-console-check.mjs cannot
// read as steps: this runs it and prints one line per finding, with the route, the rule and the text.
// usage: ORIGIN=... STATE=... [PW_MODULE=...] node console_all_pages.mjs <check-all-pages.js> <machine id>
import { readFileSync } from "node:fs"

const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
const page = await (await browser.newContext({ storageState: process.env.STATE, viewport: { width: 1280, height: 900 } })).newPage()
let failed = false
try {
  await page.goto(process.env.ORIGIN + "/")
  await page.waitForLoadState("networkidle").catch(() => {})
  await page.waitForTimeout(4000)
  await page.evaluate((g) => Object.assign(window, g), { __machine: process.argv[3] })
  const findings = await page.evaluate(readFileSync(process.argv[2], "utf8"))
  const routes = await page.evaluate(() => (window.__checkSteps ?? []).map((s) => s.step))
  const visited = routes.filter((s) => s.startsWith("visited ")).length
  console.log(`${visited >= 10 ? "PASS" : "FAIL"}  the check visited the console's pages  (${visited} routes)`)
  failed = visited < 10
  if (!Array.isArray(findings)) throw new Error("the check returned no list of findings")
  for (const f of findings) console.log(`FAIL  ${f.route}: ${f.kind}  (${f.detail.replace(/[^\s@]+@[^\s@]+/g, "<email>")})`)
  if (findings.length) failed = true
  else console.log("PASS  no page has an error, a dead link, raw text or an empty state without a next action")
} catch (error) {
  failed = true
  console.log(`FAIL  the all-pages check stopped  (${String(error.message).split("\n")[0].slice(0, 200)})`)
} finally {
  await browser.close()
}
process.exit(failed ? 1 : 0)
