// X-14: what a browser saves when each Inbox item of one machine is downloaded from the console Files screen.
// usage: ORIGIN=<console origin> STATE=<storage state> [ONLY=<regex of the item labels to download>] [PW_CHROMIUM=<path> | PW_CHANNEL=chrome|msedge] [PW_MODULE=<playwright-core index.mjs>]
//        node console_download_names.mjs <machine-id> <out.json> [OS label]
// Downloads every `a[download]` link of /files?machine=<id> through the browser's own download, saves each under the name the browser
// chose into a scratch folder of this OS (so the OS file system must accept it) and writes {item, suggested, saved, bytes} per item.
// Nothing secret is printed; the scratch folder is removed afterwards.
import { mkdtempSync, readdirSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"

const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const [machine, out, label = process.platform] = process.argv.slice(2)
const browser = await chromium.launch(process.env.PW_CHROMIUM ? { executablePath: process.env.PW_CHROMIUM } : { channel: process.env.PW_CHANNEL })
const context = await browser.newContext({ storageState: process.env.STATE, acceptDownloads: true })
const page = await context.newPage()
await page.goto(`${process.env.ORIGIN}/files?machine=${machine}`)
await page.waitForLoadState("networkidle").catch(() => {})
await page.waitForSelector("a[download]", { timeout: 60000 })
const only = process.env.ONLY ? new RegExp(process.env.ONLY) : null
const items = (await page.$$eval("a[download]", (links) => links.map((a) => a.textContent.trim()))).filter((item) => !only || only.test(item))
const scratch = mkdtempSync(join(tmpdir(), "pb-x14-"))
const rows = []
for (const item of items) {
  const exact = new RegExp("^\\s*" + item.replace(/[.*+?^${}()|[\]\\]/g, "\\$&") + "\\s*$")
  const link = page.locator("a[download]").filter({ hasText: exact }).first()
  const [download] = await Promise.all([page.waitForEvent("download", { timeout: 60000 }), link.click()])
  const suggested = download.suggestedFilename()
  let saved = null
  let bytes = 0
  try {
    await download.saveAs(join(scratch, suggested))
    saved = readdirSync(scratch).find((n) => n === suggested) ?? null
    bytes = saved ? statSync(join(scratch, saved)).size : 0
  } catch (error) {
    saved = `ERR ${String(error).slice(0, 80)}`
  }
  rows.push({ item, suggested, saved, bytes })
}
writeFileSync(out, JSON.stringify({ os: label, machine, rows }, null, 1))
rmSync(scratch, { recursive: true, force: true })
await browser.close()
console.log(`${label}: ${rows.length} items downloaded`)
