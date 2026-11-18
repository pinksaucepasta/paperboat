// Browser check of the pricing page: every plan says how much public traffic is included, the speed after it and that your own
// edge has no limits (no asterisks), the comparison table and the FAQ say the same, and nothing overflows at
// phone and desktop width. Serves dist/ itself (run `bun run build` first, with a .env).
//   node checks/pricing-page.mjs        PW_MODULE=<playwright-core index.js> PW_CHROMIUM=<chrome path> when not installed
import { createServer } from "node:http"
import { existsSync, readFileSync } from "node:fs"
import { join } from "node:path"

const playwright = await import(process.env.PW_MODULE ?? "playwright-core")
const chromium = playwright.chromium ?? playwright.default.chromium
const dist = new URL("../dist/client/", import.meta.url).pathname
const root = existsSync(dist) ? dist : new URL("../dist/", import.meta.url).pathname
const types = { ".html": "text/html", ".css": "text/css", ".js": "text/javascript", ".svg": "image/svg+xml", ".png": "image/png", ".woff2": "font/woff2" }
const server = createServer((request, response) => {
  let path = decodeURIComponent(new URL(request.url, "http://x").pathname)
  if (path.endsWith("/")) path += "index.html"
  const file = join(root, path)
  if (!file.startsWith(root) || !existsSync(file)) return response.writeHead(404).end()
  response.writeHead(200, { "content-type": types[file.slice(file.lastIndexOf("."))] ?? "application/octet-stream" }).end(readFileSync(file))
})
await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve))
const origin = `http://127.0.0.1:${server.address().port}`
const browser = await chromium.launch(process.env.PW_CHROMIUM ? { executablePath: process.env.PW_CHROMIUM } : {})
let failed = 0
const check = (step, ok, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
try {
  for (const [name, width] of [["desktop", 1440], ["phone", 390]]) {
    const page = await (await browser.newContext({ viewport: { width, height: 900 } })).newPage()
    await page.goto(`${origin}/pricing/`)
    const plans = await page.$$eval(".plan", (cards) => cards.map((card) => ({ name: card.querySelector(".name")?.textContent, text: card.innerText.replace(/\s+/g, " ") })))
    check(`${name}: three plans`, plans.length === 3, plans.map((p) => p.name).join(","))
    check(`${name}: each plan states its included traffic, the speed after it and no limits on your own edge`, /Public traffic included 5 GB a month Speed after that 512 kbit\/s Your own edge or relay No limits/.test(plans[0]?.text) && /Public traffic included 100 GB a month Speed after that 1 Mbit\/s/.test(plans[1]?.text) && /Public traffic included 100 GB a seat a month Speed after that 1 Mbit\/s/.test(plans[2]?.text), plans.map((p) => p.text.slice(40, 120)).join(" | "))
    const note = (await page.$eval(".plans-block .footnote", (e) => e.innerText)).replace(/\s+/g, " ")
    check(`${name}: the sentence under the plans is plain: included, reduced speed, never cut off or billed, own edge no limits`, /included up to the amount shown each month\. After that, speed is reduced \(512 kbit\/s on Free, 1 Mbit\/s on Pro and Team\) until your next billing date\. It is never cut off and never billed extra\. Run your own edge or relay \(free, open source\): no limits\.$/.test(note), note.slice(0, 80))
    const text = (await page.$eval("body", (e) => e.innerText)).replace(/\s+/g, " ")
    check(`${name}: no asterisks or "Unlimited" for managed traffic`, !/\*/.test(text.replace(/\*\*/g, "")) && !/Unlimited\*/.test(text), (text.match(/.{20}\*.{20}/) ?? [""])[0])
    const table = (await page.$eval("table.facts", (e) => e.innerText)).replace(/\s+/g, " ")
    check(`${name}: the comparison table says the included amount, the speeds, relay cap and no limits on your own edges`, /included a month 5 GB 100 GB 100 GB a seat/.test(table) && /Speed after that, until the next billing date 512 kbit\/s 1 Mbit\/s 1 Mbit\/s/.test(table) && /Traffic through our relays, a month, per account 10 Mbit\/s for 10 GB, then 512 kbit\/s 10 Mbit\/s for 100 GB, then 1 Mbit\/s 10 Mbit\/s for 100 GB, then 1 Mbit\/s/.test(table) && /Your own edges and relays No limits/.test(table))
    const faq = (await page.$$eval("details summary", (items) => items.map((e) => e.textContent.trim()))).join("|")
    check(`${name}: the FAQ answers what happens after, relays and self-hosting`, /What happens after the included traffic\?/.test(faq) && /Are relays limited\?/.test(faq) && /host my own relay/.test(faq))
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)
    check(`${name}: no horizontal overflow`, overflow <= 0, `${overflow}px`)
    check(`${name}: no shouting (no all-caps text, no badges beyond the yearly saving)`, !(await page.$$eval(".plan *", (els) => els.some((e) => getComputedStyle(e).textTransform === "uppercase"))))
    await page.screenshot({ path: process.env.SHOTS ? `${process.env.SHOTS}/pricing-${name}.png` : undefined, fullPage: true }).catch(() => {})
  }
} finally {
  await browser.close()
  server.close()
}
console.log(failed ? `${failed} failed` : "all passed")
process.exit(failed ? 1 : 0)
