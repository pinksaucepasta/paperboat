// F1-12 with a real pairing, and F1-01's console one-liner: check-add-machine.js with its loopback sink answered inside the
// browser. The command the dialog shows is written to COMMAND_FILE (mode 600: it holds a one-shot token, never printed);
// the caller runs it on a clean machine while the dialog waits to turn into "Machine connected".
// usage: ORIGIN=... STATE=... COMMAND_FILE=<path> [PW_MODULE=...] node console_add_machine.mjs <check-add-machine.js>
import { readFileSync, renameSync, writeFileSync } from "node:fs"

const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const origin = process.env.ORIGIN
const sink = `${origin}/__paperboat_row_sink`
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
let failed = 0
try {
  const context = await browser.newContext({ storageState: process.env.STATE, viewport: { width: 1280, height: 900 } })
  await context.route(sink, async (route) => {
    writeFileSync(process.env.COMMAND_FILE + ".tmp", route.request().postData() ?? "", { mode: 0o600 })
    renameSync(process.env.COMMAND_FILE + ".tmp", process.env.COMMAND_FILE)
    await route.fulfill({ status: 204, body: "" })
  })
  const page = await context.newPage()
  await page.goto(origin + "/")
  await page.waitForLoadState("networkidle").catch(() => {})
  await page.waitForTimeout(4000)
  await page.evaluate((g) => Object.assign(window, g), { __connect: sink, __connectWait: 600000 })
  let steps = []
  try {
    steps = (await page.evaluate(readFileSync(process.argv[2], "utf8"))) ?? []
  } catch (error) {
    steps = [...((await page.evaluate(() => window.__checkSteps).catch(() => [])) ?? []), { ok: false, step: "the check stopped", detail: String(error.message).split("\n")[0] }]
  }
  for (const s of steps) {
    if (!s.ok) failed++
    console.log(`${s.ok ? "PASS" : "FAIL"}  ${s.step}${s.detail ? `  (${String(s.detail).replace(/PB_TOKEN='[^']*'/g, "PB_TOKEN=<token>")})` : ""}`)
  }
} finally {
  await browser.close()
}
process.exit(failed || !steps.length ? 1 : 0)
