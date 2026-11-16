// After a console deploy (W-08): every page loads with the answers the real control plane sends, so none shows the
// boundary's "cannot read" error (ApiError invalid_response, lib/api-validate.ts).
//   STATE=<storage state> CHROME=<Chromium> CONSOLE_ORIGIN=<console origin> [PW_MODULE=<playwright-core index.mjs>] node check-answers-readable.mjs
// Read-only. Uses the first machine of the signed-in account for the machine page.
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const { STATE, CHROME, CONSOLE_ORIGIN: origin } = process.env
for (const [name, value] of Object.entries({ STATE, CHROME, CONSOLE_ORIGIN: origin }))
  if (!value) throw new Error(`${name} is not set; see the header of check-answers-readable.mjs`)
const browser = await chromium.launch({ executablePath: CHROME })
const ctx = await browser.newContext({ storageState: STATE, viewport: { width: 1280, height: 900 } })
const page = await ctx.newPage()
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const refused = []
page.on("console", (message) => /cannot read|invalid_response/.test(message.text()) && refused.push(message.text().slice(0, 200)))
const machines = (await (await ctx.request.get(`${origin}/api/v1/machines?workspace=personal&limit=1`)).json()).data?.items ?? []
const routes = ["/", "/terminals", "/terminals?view=recordings", "/previews", "/tunnels", "/files", "/environment", "/config", "/network", "/usage", "/activity", "/team", "/access", "/settings", "/billing", "/settings/recipes"]
if (machines[0]) routes.push(`/machines/${machines[0].id}`, `/terminals?machine=${machines[0].id}`, `/files?machine=${machines[0].id}`)
for (const route of routes) {
  refused.length = 0
  await page.goto(origin + route, { waitUntil: "networkidle", timeout: 120000 }).catch(() => {})
  await page.waitForTimeout(2500)
  const text = await page.locator("body").innerText()
  const seen = /answer this console cannot read/.test(text)
  log(!seen && !refused.length, `${route} shows no unreadable answer`, seen ? text.match(/\(([^)]*)\)\. Reload/)?.[1] : refused[0])
}
await browser.close()
console.log(failed ? `${failed} failed` : "all passed")
process.exit(failed ? 1 : 0)
