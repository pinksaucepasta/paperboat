// Mints a short-lived personal-workspace API token with a stored console session (STATE, mode 600) and writes it to stdout only.
// usage: ORIGIN=<console origin> STATE=<storage state> PW_CHROMIUM=<chromium> [PW_MODULE=<playwright-core index.mjs>] node mint_api_token.mjs <name>
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const origin = process.env.ORIGIN
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
const context = await browser.newContext({ storageState: process.env.STATE })
const response = await context.request.post(`${origin}/api/v1/workspaces/personal/api-tokens`, {
  headers: { origin, "content-type": "application/json", "idempotency-key": crypto.randomUUID() },
  data: { name: process.argv[2], expires_in_days: 1 },
})
const body = await response.json()
await browser.close()
const secret = body?.data?.secret ?? body?.data?.token
if (!response.ok() || !secret) {
  console.error("api token not created: HTTP " + response.status() + " " + JSON.stringify(body?.error ?? {}).slice(0, 200))
  process.exit(1)
}
process.stdout.write(secret)
