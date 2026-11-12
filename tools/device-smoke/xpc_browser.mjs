// Opens a URL in a real (headless) browser and checks the page: usage  node xpc_browser.mjs URL MARKER
// env: PW_MODULE (playwright-core index.mjs), PW_CHROMIUM (browser executable) or PW_CHANNEL (chrome, msedge)
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const [url, marker] = process.argv.slice(2)
const launch = process.env.PW_CHROMIUM ? { executablePath: process.env.PW_CHROMIUM } : { channel: process.env.PW_CHANNEL }
const browser = await chromium.launch({ ...launch, headless: true })
try {
  const page = await browser.newPage()
  const response = await page.goto(url, { waitUntil: "load", timeout: 60000 })
  const text = (await page.textContent("body")) ?? ""
  console.log(JSON.stringify({ status: response.status(), marker: text.includes(marker), browser: browser.version(), secure: url.startsWith("https://") }))
} finally {
  await browser.close()
}
