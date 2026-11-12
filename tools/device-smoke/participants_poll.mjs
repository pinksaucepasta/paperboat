// F21-169: a viewer's browser tab learns a shared terminal's participants by event, not by polling every 4 s.
//
// node participants_poll.mjs --machine <id> --state <storage-state file> [--seconds 180]
// Env: PB_CONSOLE_ORIGIN, PW_CHROMIUM. Opens one new browser terminal on the machine, waits until it is attached, then counts
// the console's API requests per path for --seconds and checks that the participants/sharing request is made at most once per
// minute (plus one at load) instead of every 4 s (45 in 180 s). Closes the terminal at the end. Prints one JSON receipt.
import { chromium } from "playwright-core"

const arg = (name, fallback = "") => {
  const i = process.argv.indexOf(`--${name}`)
  return i > 0 ? process.argv[i + 1] : fallback
}
const origin = process.env.PB_CONSOLE_ORIGIN
const exe = process.env.PW_CHROMIUM
const machine = arg("machine")
const seconds = Number(arg("seconds", "180"))
if (!origin || !exe || !machine || !arg("state")) throw new Error("set PB_CONSOLE_ORIGIN, PW_CHROMIUM, --machine and --state")

const browser = await chromium.launch({ executablePath: exe })
const context = await browser.newContext({ storageState: arg("state") })
const page = await context.newPage()
const counts = {}
const idPath = (url) => new URL(url).pathname.replace(/[0-9a-f]{8}-[0-9a-f-]{27,}|(machine|terminal|session)_[0-9a-zA-Z-]+/g, "$1ID")
let counting = false
page.on("request", (request) => {
  const url = request.url()
  if (counting && url.startsWith(origin) && new URL(url).pathname.startsWith("/api/")) counts[idPath(url)] = (counts[idPath(url)] ?? 0) + 1
})
let id = ""
let receipt
try {
  await page.goto(`${origin}/terminals/new?machine=${encodeURIComponent(machine)}&workspace=personal`)
  await page.waitForURL(/\/terminals\/(?!new)[^/?]+/, { timeout: 90_000 })
  id = page.url().match(/\/terminals\/([^/?]+)/)?.[1] ?? ""
  await page.waitForSelector('[data-terminal-phase="attached"]', { timeout: 90_000 })
  counting = true
  await page.waitForTimeout(seconds * 1000)
  counting = false
  const sharing = Object.entries(counts).filter(([path]) => /sharing|participants/.test(path))
  const total = sharing.reduce((sum, [, n]) => sum + n, 0)
  const allowed = Math.ceil(seconds / 60) + 1
  receipt = { check: "participants-poll", status: total <= allowed ? "PASS" : "FAIL", seconds, participants_requests: total, allowed, by_path: counts }
} finally {
  if (id) {
    try {
      await page.getByRole("button", { name: "Close terminal" }).first().click({ timeout: 15_000 })
      await page.getByRole("alertdialog").getByRole("button", { name: "Close terminal" }).click({ timeout: 15_000 })
      await page.waitForURL(/\/terminals$/, { timeout: 20_000 })
    } catch {
      console.error(`cleanup: could not close terminal ${id}`)
    }
  }
  await browser.close()
}
console.log(JSON.stringify(receipt))
process.exit(receipt.status === "PASS" ? 0 : 1)
