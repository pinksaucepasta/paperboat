// Browser check of the abuse report form (W-09): builds the site against a test API, serves it, and drives /report-abuse/.
//   CHROME=<Chromium> [PW_MODULE=<playwright-core index.mjs>] node checks/report-abuse.mjs      (from marketing/, after `bun install`)
// The test API answers GET /v1/client-configuration (with the abuse mailbox) and POST /v1/abuse-reports: a link containing
// "limited" gets 429 with Retry-After, "broken" gets 503, "invalid" gets 400 naming the field, anything else 202 with an id.
// Nothing real is contacted, nothing is written to a real server.
import { createServer } from "node:http"
import { readFileSync, mkdtempSync, existsSync, rmSync } from "node:fs"
import { tmpdir } from "node:os"
import { join, extname } from "node:path"
import { spawn } from "node:child_process"

const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const mailbox = "abuse@mail.example.test"
const received = []
const listen = (server) => new Promise((resolve) => server.listen(0, "127.0.0.1", () => resolve(server.address().port)))

const api = createServer(async (req, res) => {
  const cors = { "access-control-allow-origin": "*", "access-control-allow-headers": "content-type", "access-control-expose-headers": "Retry-After" }
  const send = (status, body, extra = {}) => {
    res.writeHead(status, { "content-type": "application/json", ...cors, ...extra })
    res.end(JSON.stringify(body))
  }
  if (req.method === "OPTIONS") return (res.writeHead(204, { ...cors, "access-control-allow-methods": "POST" }), res.end())
  if (req.url === "/v1/client-configuration")
    return send(200, { data: { console_url: "https://console.example.test", installer_base_url: "https://install.example.test", local_access_base_domain: "local.example.test", preview_base_domain: "preview.example.test", tunnel_base_domain: "tunnel.example.test", abuse_mailbox: mailbox } })
  if (req.url === "/v1/abuse-reports" && req.method === "POST") {
    let raw = ""
    for await (const chunk of req) raw += chunk
    const report = JSON.parse(raw)
    received.push(report)
    if (report.url.includes("limited")) return send(429, { error: { code: "rate_limited", message: "Too many attempts from your network." } }, { "retry-after": "42" })
    if (report.url.includes("broken")) return send(503, { error: { code: "abuse_report_unavailable", message: "The report could not be recorded." } })
    if (report.url.includes("invalid")) return send(400, { error: { code: "invalid_request", message: "reason is not one of the listed reasons." } })
    return send(202, { data: { id: "abuse_test_1" } })
  }
  send(404, { error: { code: "not_found", message: "no" } })
})
const apiPort = await listen(api)
const siteUrl = `http://127.0.0.1:${apiPort}`

const out = mkdtempSync(join(tmpdir(), "report-abuse-"))
// Async: the build reads the client configuration from the test API above, which this process serves.
const build = await new Promise((resolve) => {
  const child = spawn("bunx", ["astro", "build", "--outDir", out], { env: { ...process.env, PAPERBOAT_API_URL: siteUrl, PAPERBOAT_SITE_URL: siteUrl } })
  let log = ""
  child.stdout.on("data", (d) => (log += d))
  child.stderr.on("data", (d) => (log += d))
  child.on("close", (status) => resolve({ status, log }))
})
if (build.status !== 0) throw new Error(`site build failed:\n${build.log}`)

const types = { ".html": "text/html", ".css": "text/css", ".js": "text/javascript", ".svg": "image/svg+xml" }
const site = createServer((req, res) => {
  let path = join(out, new URL(req.url, siteUrl).pathname)
  if (!extname(path)) path = join(path, "index.html")
  if (!existsSync(path)) return (res.writeHead(404), res.end())
  res.writeHead(200, { "content-type": types[extname(path)] ?? "application/octet-stream" })
  res.end(readFileSync(path))
})
const sitePort = await listen(site)
const origin = `http://127.0.0.1:${sitePort}`

let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const browser = await chromium.launch({ executablePath: process.env.CHROME })
const page = await (await browser.newContext({ viewport: { width: 1280, height: 900 } })).newPage()
await page.goto(`${origin}/report-abuse/`)
log((await page.locator("h1").innerText()) === "Report abuse", "the page is headed Report abuse")
log((await page.locator(`a[href="mailto:${mailbox}"]`).count()) === 1, "it shows the abuse mailbox from the client configuration")
log((await page.locator('footer a[href="/report-abuse/"]').count()) === 1, "the footer links to it")
const reasons = await page.locator("select[name=reason] option").evaluateAll((o) => o.map((x) => x.value))
log(reasons.join(",") === "phishing,malware,spam,illegal,other", "the reason is a closed list", reasons.join(","))

const status = page.locator("#status")
const submit = async (url, reason = "phishing", details = "") => {
  await page.fill("input[name=url]", url)
  await page.selectOption("select[name=reason]", reason)
  await page.fill("textarea[name=details]", details)
  await page.click("button[type=submit]")
  await status.filter({ hasText: /./ }).waitFor()
  await page.waitForFunction(() => !/Sending/.test(document.querySelector("#status").textContent))
  return status.innerText()
}

log(/https?:\/\//.test(await submit("not a link")) && received.length === 0, "a link that is not http or https is refused before sending")
let text = await submit("https://quiet-river-1234.preview.example.test/login", "malware", "asks for a password")
log(/abuse_test_1/.test(text) && status.getAttribute("data-state") !== null, "an accepted report shows its reference", text)
log(received.at(-1)?.url === "https://quiet-river-1234.preview.example.test/login" && received.at(-1)?.reason === "malware" && received.at(-1)?.details === "asks for a password", "the server received the link, reason and details")
log((await page.inputValue("input[name=url]")) === "", "the form is cleared after a report")
text = await submit("https://x.example.test/limited")
log(/Too many reports/.test(text) && /42 seconds/.test(text) && text.includes(mailbox), "rate limit: says so, how long to wait, and offers the mailbox", text)
text = await submit("https://x.example.test/broken")
log(/could not be recorded/.test(text) && text.includes(mailbox), "server error: says it was not recorded and offers the mailbox", text)
text = await submit("https://x.example.test/invalid")
log(/reason is not one of the listed reasons/.test(text), "invalid report: shows the server's message naming the field", text)
log(!(await page.locator("button[type=submit]").isDisabled()), "the form can be sent again after an error")
await api.close()
text = await submit("https://x.example.test/offline")
log(/could not be sent/.test(text) && text.includes(mailbox), "network failure: says it was not sent and offers the mailbox", text)

await browser.close()
site.close()
rmSync(out, { recursive: true, force: true })
console.log(failed ? `${failed} failed` : "all passed")
process.exit(failed ? 1 : 0)
