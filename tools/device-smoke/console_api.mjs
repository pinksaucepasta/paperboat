// One console API call with a stored console session (STATE, mode 600): prints {"status":N,"body":...} on one line.
// usage: ORIGIN=<console origin> STATE=<storage state> PW_CHROMIUM=<chromium> [PW_MODULE=<playwright-core index.mjs>] node console_api.mjs GET|POST|PUT|DELETE /v1/path [json body]
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const [method, path, body] = process.argv.slice(2)
const origin = process.env.ORIGIN
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
const context = await browser.newContext({ storageState: process.env.STATE })
const reply = await context.request.fetch(`${origin}/api${path}`, {
  method,
  headers: { origin, "content-type": "application/json", "idempotency-key": crypto.randomUUID() },
  data: body ? JSON.parse(body) : undefined,
})
const text = await reply.text()
await browser.close()
let parsed
try { parsed = JSON.parse(text) } catch { parsed = text.slice(0, 200) }
console.log(JSON.stringify({ status: reply.status(), body: parsed }))
