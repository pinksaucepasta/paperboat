// PostHog events of the console, end to end against the live console (W-08 follow-up): pairs a machine through the Add machine
// dialog and creates a preview through the New preview dialog, and checks that the browser sent `add_machine_started`,
// `machine_paired` and `preview_created` to the analytics host and that the host accepted each (HTTP 200).
//   STATE=<storage state> CHROME=<Chromium> CONSOLE_ORIGIN=<console origin> POSTHOG_HOST=<the console build's NEXT_PUBLIC_POSTHOG_HOST>
//   PAIR_CMD=<shell command that pairs a machine named console6-ph using the token in $TOKEN_FILE and serves HTTP on 18088 there>
//   [PW_MODULE=<playwright-core index.mjs>] node check-analytics.mjs
// The token goes to PAIR_CMD only through a mode-600 file that is deleted afterwards; nothing secret is printed. The machine is
// revoked at the end (exact id). Reuses check-add-machine.js and check-previews.js for the dialogs' own steps.
import { spawn } from "node:child_process"
import { createServer } from "node:http"
import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"

const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const { STATE, CHROME, CONSOLE_ORIGIN: origin, POSTHOG_HOST: host, PAIR_CMD: pairCmd } = process.env
for (const [name, value] of Object.entries({ STATE, CHROME, CONSOLE_ORIGIN: origin, POSTHOG_HOST: host, PAIR_CMD: pairCmd }))
  if (!value) throw new Error(`${name} is not set; see the header of check-analytics.mjs`)

let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const scripts = new URL("./", import.meta.url)

// The Add machine dialog sends its pairing command here; the token is read from the command (PB_TOKEN) and handed to PAIR_CMD in a file.
const dir = mkdtempSync(join(tmpdir(), "analytics-"))
const sink = createServer((request, response) => {
  let body = ""
  request.on("data", (chunk) => (body += chunk))
  request.on("end", async () => {
    response.writeHead(204, { "access-control-allow-origin": "*" }).end()
    try {
      const token = body.match(/PB_TOKEN='([A-Za-z0-9]+)'/)[1]
      const file = join(dir, "token")
      writeFileSync(file, token, { mode: 0o600 })
      chmodSync(file, 0o600)
      const child = spawn("bash", ["-c", pairCmd], { env: { ...process.env, TOKEN_FILE: file }, stdio: ["ignore", "ignore", "inherit"] })
      child.on("close", () => rmSync(file, { force: true }))
    } catch (error) {
      console.log(`FAIL  could not hand the pairing token to PAIR_CMD (${error.message})`)
      failed++
    }
  })
})
await new Promise((resolve) => sink.listen(0, "127.0.0.1", resolve))

const browser = await chromium.launch({ executablePath: CHROME })
const page = await (await browser.newContext({ storageState: STATE, viewport: { width: 1280, height: 900 } })).newPage()
const events = []
page.on("response", async (response) => {
  if (!response.url().startsWith(new URL("/i/v0/e/", host).href)) return
  const sent = JSON.parse(response.request().postData() ?? "{}")
  events.push({ event: sent.event, status: response.status(), properties: Object.keys(sent.properties ?? {}).sort().join(",") })
})

const run = async (path, script, globals) => {
  await page.goto(origin + path, { waitUntil: "networkidle", timeout: 120000 })
  await page.waitForTimeout(3000)
  await page.evaluate((g) => Object.assign(window, g), globals)
  const steps = await page.evaluate(readFileSync(new URL(script, scripts), "utf8"))
  for (const step of steps ?? []) log(step.ok, `${script}: ${step.step}`, step.detail)
  return page.evaluate(() => window.__machineId)
}

const machineId = await run("/", "check-add-machine.js", { __connect: `http://127.0.0.1:${sink.address().port}` })
await run("/previews", "check-previews.js", { __machine: "console6-ph", __port: "18088" })

// The three events, each accepted by the analytics host.
await page.waitForTimeout(2000)
for (const name of ["add_machine_started", "machine_paired", "preview_created"]) {
  const seen = events.filter((e) => e.event === name)
  log(seen.length >= 1 && seen.every((e) => e.status === 200), `PostHog accepted ${name}`, seen.map((e) => `HTTP ${e.status}, properties ${e.properties}`).join("; "))
}

// Cleanup: revoke the machine this run paired, by its exact id.
if (machineId) {
  const status = await page.evaluate(
    async (id) => (await fetch(`/api/v1/machines/${id}`, { method: "DELETE", headers: { "idempotency-key": crypto.randomUUID().replaceAll("-", "") } })).status,
    machineId,
  )
  log(status >= 200 && status < 300, "the machine this run paired is revoked", `HTTP ${status}`)
}
await browser.close()
sink.close()
rmSync(dir, { recursive: true, force: true })
console.log(failed ? `${failed} failed` : "all passed")
process.exit(failed ? 1 : 0)
