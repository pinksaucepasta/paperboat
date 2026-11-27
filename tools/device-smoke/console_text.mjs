// Loads one console page with the stored session and checks that its text matches each pattern.
// usage: ORIGIN=... STATE=... [PW_MODULE=...] node console_text.mjs /path <regexp>...
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const [path, ...patterns] = process.argv.slice(2)
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
let failed = 0
try {
  const page = await (await browser.newContext({ storageState: process.env.STATE, viewport: { width: 1280, height: 900 } })).newPage()
  await page.goto(process.env.ORIGIN + path, { waitUntil: "domcontentloaded", timeout: 120000 })
  await page.waitForTimeout(8000)
  // some pages (Delete account) stand on their own, without the console's <main>
  const text = (await ((await page.locator("main").count()) ? page.locator("main").last() : page.locator("body")).innerText()).replace(/\s+/g, " ").replace(/[^\s@]+@[^\s@]+/g, "<email>")
  for (const pattern of patterns) {
    const ok = new RegExp(pattern).test(text)
    if (!ok) failed++
    console.log(`${ok ? "PASS" : "FAIL"}  ${path} says ${pattern}  (${text.slice(0, 300)})`)
  }
} finally {
  await browser.close()
}
process.exit(failed ? 1 : 0)
