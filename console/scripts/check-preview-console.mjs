// Browser check: a preview started from the console goes live, loads, and its Inspector shows requests, on each
// machine given. For every <machine name>:<port> (an app must already listen on that port on the machine): runs
// check-previews.js on /previews (New preview dialog, Live, the address answers) keeping the preview, then
// check-preview-inspector.js on its Inspector page, then stops that preview by exact id. Prints PASS/FAIL per step;
// exits 1 when a step failed.
// usage: ORIGIN=<console origin> STATE=<playwright storage state of the owner> PW_CHROMIUM=<chromium path>
//        [PW_MODULE=<playwright-core index.mjs>] node check-preview-console.mjs <machine>:<port> [<machine>:<port> ...]
import { readFileSync } from "node:fs"

// ESM ignores NODE_PATH: PW_MODULE is the path of playwright-core's index.mjs when it is not installed next to this file.
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")

const here = new URL(".", import.meta.url).pathname
const createCheck = readFileSync(`${here}check-previews.js`, "utf8")
const inspectorCheck = readFileSync(`${here}check-preview-inspector.js`, "utf8")
const targets = process.argv.slice(2)
if (!targets.length || targets.some((t) => !/^.+:\d+$/.test(t))) {
  console.error("usage: node check-preview-console.mjs <machine>:<port> [<machine>:<port> ...]")
  process.exit(2)
}
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
const context = await browser.newContext({ storageState: process.env.STATE })
let passed = 0
let failed = 0
const print = (machine, steps) => {
  for (const s of steps ?? []) {
    if (s.ok) passed++
    else failed++
    console.log(`${s.ok ? "PASS" : "FAIL"}  ${machine}: ${s.step}${s.detail ? `  (${s.detail})` : ""}`)
  }
  if (!steps?.length) {
    failed++
    console.log(`FAIL  ${machine}: the check returned no steps`)
  }
}
// Opens path, sets the globals, runs a paste-in check and returns its steps (and the page for follow-up reads).
const run = async (path, globals, script) => {
  const page = await context.newPage()
  await page.goto(process.env.ORIGIN + path)
  await page.waitForLoadState("networkidle").catch(() => {})
  await page.waitForTimeout(4000)
  await page.evaluate((g) => Object.assign(window, g), globals)
  return { page, steps: await page.evaluate(script).catch((error) => [{ step: "check ran", ok: false, detail: String(error) }]) }
}

for (const target of targets) {
  const [, machine, port] = /^(.+):(\d+)$/.exec(target)
  const created = await run("/previews", { __machine: machine, __port: port, __keep: true }, createCheck)
  print(machine, created.steps)
  const id = await created.page.evaluate(() => window.__previewId).catch(() => undefined)
  const address = await created.page.evaluate(() => window.__previewAddress).catch(() => undefined)
  if (id && address) {
    const inspected = await run(`/previews/${encodeURIComponent(id)}/inspect`, { __previewHost: address }, inspectorCheck)
    print(machine, inspected.steps)
    await inspected.page.close()
  }
  if (id) {
    // Stop exactly the preview this check started (the lease's ETag comes from reading it).
    const stopped = await created.page.evaluate(async (id) => {
      const path = `/api/v1/previews/${encodeURIComponent(id)}`
      const read = await fetch(path, { headers: { accept: "application/json" } })
      const etag = read.headers.get("etag")
      if (!etag) return read.status === 404
      const stop = await fetch(path, { method: "DELETE", headers: { "if-match": etag, "idempotency-key": crypto.randomUUID().replaceAll("-", "") } })
      return stop.ok
    }, id)
    print(machine, [{ step: "the check's preview is stopped", ok: stopped, detail: id }])
  }
  await created.page.close()
}
console.log(`${passed}/${passed + failed} passed`)
await browser.close()
process.exit(failed ? 1 : 0)
