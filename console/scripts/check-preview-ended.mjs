// A preview whose lease ended shows as ended in the console: the API reports endpoint_state "ended" with no
// address, the Ended list shows its row with no link and no URL, and the Live list does not carry it.
// The preview is a real one that was started and stopped on a machine (pb preview <port> --background, pb preview stop).
//   STATE=<storage state> CHROME=<Chromium> CONSOLE_ORIGIN=<console origin> PREVIEW=<preview id> TARGET=<its "Forwards to" text, e.g. localhost:8099> node check-preview-ended.mjs
import { chromium } from "playwright-core"

const { STATE, CHROME, CONSOLE_ORIGIN: origin, PREVIEW: id, TARGET: target } = process.env
const browser = await chromium.launch({ executablePath: CHROME })
const ctx = await browser.newContext({ storageState: STATE, viewport: { width: 1280, height: 800 } })
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}

const preview = (await (await ctx.request.get(`${origin}/api/v1/previews/${encodeURIComponent(id)}`)).json()).data
log(preview?.endpoint_state === "ended" && preview.endpoint === "", "the server reports the ended lease as ended, with no address", `${preview?.state} ${preview?.endpoint_state}`)

const page = await ctx.newPage()
await page.goto(`${origin}/previews`, { waitUntil: "networkidle", timeout: 120000 })
log((await page.getByText(target).count()) === 0, "the Live list does not carry it")
await page.getByRole("radio", { name: /Ended/ }).or(page.getByRole("button", { name: /Ended/ })).first().click()
const row = page.locator("tr", { hasText: target }).first()
await row.waitFor({ timeout: 30000 })
const address = row.locator("td").first()
log((await address.locator("a").count()) === 0, "the Ended row's address is not a link")
const text = (await address.innerText()).trim()
log(text === "Ended" && !/https?:|\./.test(text), "...and shows no stale URL", text)
log(!(await row.innerText()).includes("Live"), "...and no 'Live' status")
await page.screenshot({ path: "/tmp/preview-ended.png" })
await browser.close()
console.log(failed ? `${failed} failed` : "all passed")
process.exit(failed ? 1 : 0)
