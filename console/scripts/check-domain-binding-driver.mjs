// F8-02 driver: runs check-domain-binding.js on a tunnel page and creates/deletes the DNS records it asks for through the fixtures endpoint.
// usage: ORIGIN=<console origin> STATE=<storage state of the owner> FINAL_FIXTURES_URL=<fixtures endpoint> FINAL_FIXTURES_TOKEN_FILE=<its token file> CHECK=console/scripts/check-domain-binding.js node check-domain-binding-driver.mjs <tunnelId> <fresh hostname> <zone>
// (the tunnel needs its service up; removed hostnames are held for a week, so use a new hostname each run)
// ESM ignores NODE_PATH: PW_MODULE is the path of playwright-core's index.mjs when it is not installed next to this file.
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
import { readFileSync } from "node:fs"
const [tunnelId, domain, zoneName] = process.argv.slice(2)
const origin = process.env.ORIGIN
const token = readFileSync(process.env.FINAL_FIXTURES_TOKEN_FILE, "utf8").trim()
const dns = async (method, path, body) => {
  const r = await fetch(`${process.env.FINAL_FIXTURES_URL.replace(/\/$/, "")}${path}`, { method, headers: { authorization: `Bearer ${token}`, "content-type": "application/json" }, body: body ? JSON.stringify(body) : undefined })
  if (!r.ok) throw new Error(`fixtures ${method} ${path}: HTTP ${r.status}`)
  return r.status === 204 ? null : (await r.json()).data
}
const created = []
const exe = process.env.PW_CHROMIUM ?? `${process.env.HOME}/.cache/ms-playwright/chromium_headless_shell-1248/chrome-headless-shell-linux-arm64/chrome-headless-shell`
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
      const res = await dns("POST", "/dns", { type: r.type, name, content: r.value.replace(/\.$/, "") })
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
  for (const id of created) await dns("DELETE", `/dns/${id}`).catch((e) => console.log("cleanup failed", id, e.message))
  console.log(`dns records deleted: ${created.length}`)
  await browser.close()
}
process.exit(failed ? 1 : 0)
