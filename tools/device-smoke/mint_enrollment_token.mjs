// Writes a one-shot machine enrollment token of the stored console session's account to stdout (STATE: Playwright storage state, mode 600).
// CANCEL=1 revokes the token again before writing it (what a refused revoked token looks like).
// usage: ORIGIN=<console origin> STATE=<storage state> [PW_MODULE=<playwright-core index.mjs>] node mint_enrollment_token.mjs
const { request } = await import(process.env.PW_MODULE ?? "playwright-core")
const origin = process.env.ORIGIN
// An API request context carries the stored session's cookies without starting a browser (a host runs one at a time).
const context = await request.newContext({ storageState: process.env.STATE })
const response = await context.post(`${origin}/api/v1/machine-enrollments`, {
  headers: { origin, "content-type": "application/json", "idempotency-key": crypto.randomUUID() },
  data: {},
})
const body = await response.json()
let cancelled
if (process.env.CANCEL === "1" && response.ok() && body?.data?.id) {
  cancelled = await context.post(`${origin}/api/v1/machine-enrollments/${encodeURIComponent(body.data.id)}/cancel`, {
    headers: { origin, "idempotency-key": crypto.randomUUID() },
  })
}
await context.dispose()
if (!response.ok() || !body?.data?.bootstrap_token) {
  console.error("enrollment not created: HTTP " + response.status() + " " + JSON.stringify(body?.error ?? {}).slice(0, 200))
  process.exit(1)
}
if (process.env.CANCEL === "1" && !cancelled?.ok()) {
  console.error("enrollment not revoked: HTTP " + (cancelled ? cancelled.status() : "none (no enrollment id)"))
  process.exit(1)
}
process.stdout.write(body.data.bootstrap_token)
