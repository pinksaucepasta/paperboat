// F8-02 driver: runs check-domain-binding.js on a tunnel page and creates/deletes the DNS records it asks for in Cloudflare.
// usage: ORIGIN=<console origin> STATE=<storage state of the owner> CF_TOKEN_FILE=<dns token file> CHECK=console/scripts/check-domain-binding.js node check-domain-binding-driver.mjs <tunnelId> <fresh hostname> <zone>
// (the tunnel needs its service up; removed hostnames are held for a week, so use a new hostname each run)
import { chromium } from "playwright-core"
import { readFileSync } from "node:fs"
const [tunnelId, domain, zoneName] = process.argv.slice(2)
const origin = process.env.ORIGIN
const token = readFileSync(process.env.CF_TOKEN_FILE, "utf8").trim()
const cf = async (method, path, body) => {
  const r = await fetch(`https://api.cloudflare.com/client/v4${path}`, { method, headers: { authorization: `Bearer ${token}`, "content-type": "application/json" }, body: body ? JSON.stringify(body) : undefined })
  const j = await r.json()
  if (!j.success) throw new Error(`cloudflare ${method} ${path}: ${JSON.stringify(j.errors).slice(0, 200)}`)
  return j.result
}
const zone = (await cf("GET", `/zones?name=${zoneName}`))[0].id
const created = []
const exe = `${process.env.HOME}/.cache/ms-playwright/chromium_headless_shell-1248/chrome-headless-shell-linux-arm64/chrome-headless-shell`
const browser = await chromium.launch({ executablePath: exe })
let failed = 1
try {
  const page = await (await browser.newContext({ storageState: process.env.STATE })).newPage()
  await page.goto(`${origin}/tunnels/${tunnelId}`)
  await page.waitForLoadState("networkidle").catch(() => {})
  await page.waitForTimeout(4000)
  await page.evaluate((d) => { window.__domain = d }, domain)
  const done = page.evaluate(readFileSync(process.env.CHECK, "utf8"))
  let records = null
  for (let i = 0; i < 240 && !records; i++) { records = await page.evaluate(() => window.__dnsRecords ?? null); if (!records) await page.waitForTimeout(1000) }
  if (records) {
    for (const r of records) {
      const name = r.name.endsWith(zoneName) ? r.name.replace(/\.$/, "") : `${r.name.replace(/\.$/, "")}.${zoneName}`
      const res = await cf("POST", `/zones/${zone}/dns_records`, { type: r.type, name, content: r.value.replace(/\.$/, ""), ttl: 60, proxied: false })
      created.push(res.id)
      console.log(`dns ${r.type} ${name} created`)
    }
    await page.evaluate(() => { window.__dnsReady = true })
  }
  for (let i = 0; i < 240 && !(await page.evaluate(() => window.__domainReady)); i++) await page.waitForTimeout(1000)
  const served = await fetch(`https://${domain}/`, { headers: { connection: "close" }, signal: AbortSignal.timeout(15000) }).then(async (r) => `HTTP ${r.status}, ${(await r.text()).slice(0, 40).replace(/\s+/g, " ")}`, (e) => `FAILED ${e.cause?.code ?? e.message}`)
  console.log(`${served.startsWith("HTTP 200") ? "PASS" : "FAIL"}  outside the page, https://${domain}/ serves over verified TLS  (${served})`)
  await page.evaluate(() => { window.__removeNow = true })
  const steps = await done
  failed = 0
  for (const s of steps ?? []) { if (!s.ok) failed++; console.log(`${s.ok ? "PASS" : "FAIL"}  ${s.step}${s.detail ? `  (${s.detail})` : ""}`) }
  console.log(`${(steps ?? []).length - failed}/${(steps ?? []).length} passed`)
  const url = `https://${domain}/`
  const t0 = Date.now()
  let last = ""
  for (let i = 0; i < 40; i++) {
    last = await fetch(url, { headers: { connection: "close" }, signal: AbortSignal.timeout(10000) }).then((r) => `HTTP ${r.status}`, (e) => `no connection (${e.cause?.code ?? e.message})`)
    console.log(`after removal +${Math.round((Date.now() - t0) / 1000)}s: ${last}`)
    if (!last.startsWith("HTTP 200")) break
    await page.waitForTimeout(3000)
  }
} finally {
  for (const id of created) await cf("DELETE", `/zones/${zone}/dns_records/${id}`).catch((e) => console.log("cleanup failed", id, e.message))
  console.log(`dns records deleted: ${created.length}`)
  await browser.close()
}
process.exit(failed ? 1 : 0)
