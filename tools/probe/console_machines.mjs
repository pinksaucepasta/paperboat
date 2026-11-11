// Disconnects (revokes) every machine whose alias starts with PREFIX through the stored console session (STATE, mode 600), by exact id,
// the same call the CLI's `pb machine revoke` makes (POST /v1/machines/{id}/disconnect). Then lists again: any left connected fails.
// Prints the number revoked. usage: ORIGIN=<console origin> STATE=<storage state> PREFIX=canary-pair- PW_CHROMIUM=<chromium> [PW_MODULE=...] node console_machines.mjs
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const origin = process.env.ORIGIN
const prefix = process.env.PREFIX
if (!prefix || prefix.length < 6) throw new Error("PREFIX is required (a short prefix could match real machines)")
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
const context = await browser.newContext({ storageState: process.env.STATE })
const connected = async () => {
  const response = await context.request.get(`${origin}/api/v1/machines?sort=alias&q=${encodeURIComponent(prefix)}`)
  if (!response.ok()) throw new Error(`machine list: HTTP ${response.status()}`)
  const items = (await response.json()).data?.items ?? []
  return items.filter((m) => String(m.alias).startsWith(prefix) && m.state !== "disconnected")
}
let revoked = 0
try {
  for (const machine of await connected()) {
    const response = await context.request.post(`${origin}/api/v1/machines/${encodeURIComponent(machine.id)}/disconnect`, {
      headers: { origin, "content-type": "application/json", "idempotency-key": crypto.randomUUID() },
    })
    if (!response.ok()) throw new Error(`disconnect ${machine.id}: HTTP ${response.status()}`)
    revoked++
  }
  const left = await connected()
  if (left.length) throw new Error(`${left.length} ${prefix}* machine(s) still connected`)
  console.log(`revoked ${revoked}`)
} finally {
  await browser.close()
}
