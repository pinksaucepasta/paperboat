// Writes a one-shot machine enrollment token of the stored console session's account to stdout (STATE: Playwright storage state, mode 600).
// usage: ORIGIN=<console origin> STATE=<storage state> PW_CHROMIUM=<chromium> [PW_MODULE=<playwright-core index.mjs>] node mint_enrollment_token.mjs
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const origin = process.env.ORIGIN
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
const context = await browser.newContext({ storageState: process.env.STATE })
const response = await context.request.post(`${origin}/api/v1/machine-enrollments`, {
  headers: { origin, "content-type": "application/json", "idempotency-key": crypto.randomUUID() },
  data: { shell: "posix" },
})
const body = await response.json()
await browser.close()
if (!response.ok() || !body?.data?.bootstrap_token) {
  console.error("enrollment not created: HTTP " + response.status() + " " + JSON.stringify(body?.error ?? {}).slice(0, 200))
  process.exit(1)
}
process.stdout.write(body.data.bootstrap_token)
