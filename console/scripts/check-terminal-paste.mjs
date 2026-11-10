// Headless check: pasting a small file into a browser terminal uploads it and pastes its path (F3 file paste).
// Run with playwright-core installed:
//   STATE=<storage state> CHROME=<Chromium> CONSOLE_ORIGIN=<console origin> MACHINE=<machine id> [MACHINE_SHELL=powershell] node check-terminal-paste.mjs
// A paste dispatched with a File in its clipboardData reaches the terminal's real paste handler. It passes only when
// the page says "Files uploaded and their paths pasted." and `cat <pasted path>` prints the file's content marker.
import { chromium } from "playwright-core"

const { STATE, CHROME, CONSOLE_ORIGIN: origin, MACHINE: machine } = process.env
const browser = await chromium.launch({ executablePath: CHROME })
const ctx = await browser.newContext({ storageState: STATE, ignoreHTTPSErrors: true, viewport: { width: 1200, height: 760 } })
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const waitFor = (p, fn, arg, ms) => p.waitForFunction(fn, arg, { timeout: ms }).then(() => true, () => false)
const phaseAttached = (p) =>
  waitFor(p, () => document.querySelector("[data-terminal-phase]")?.getAttribute("data-terminal-phase") === "attached", null, 90000)
const shows = (p, text, ms = 20000) =>
  waitFor(p, (t) => (document.querySelector(".terminal-host")?.innerText ?? "").includes(t), text, ms)

const p = await ctx.newPage()
await p.goto(`${origin}/terminals/new?machine=${machine}&workspace=personal`, { timeout: 280000 })
await p.waitForURL(/\/terminals\/(?!new)[^/?]+/, { timeout: 60000 }).catch(() => {})
const id = p.url().match(/\/terminals\/([^/?]+)/)?.[1]
log(!!id && id !== "new", "a terminal opens", id)
try {
  log(await phaseAttached(p), "the terminal attaches")
  const content = `PASTE-${Date.now().toString(36)}`
  await p.locator(".terminal-host").click()
  await p.keyboard.type("cat ", { delay: 15 })
  await p.evaluate((text) => {
    const data = new DataTransfer()
    data.items.add(new File([text], "paste-check.txt", { type: "text/plain" }))
    const target = document.querySelector(".terminal-host textarea") ?? document.querySelector(".terminal-host")
    target.dispatchEvent(new ClipboardEvent("paste", { clipboardData: data, bubbles: true, cancelable: true }))
  }, content)
  // The notice stays 10 s: read it as soon as it shows.
  const noticeText = await p
    .waitForFunction(() => document.querySelector('[role="status"] span.flex-1')?.textContent || false, null, { timeout: 60000 })
    .then((handle) => handle.jsonValue(), () => "")
  const said = noticeText === "Files uploaded and their paths pasted."
  log(said, "the page reports the upload and the pasted path", said ? "" : noticeText || "no notice")
  if (said) {
    await p.keyboard.press("Enter")
    log(await shows(p, content), "cat of the pasted path prints the file's content")
  }
  await p.screenshot({ path: "/tmp/terminal-paste.png" })
} finally {
  await p.getByRole("button", { name: "Close terminal" }).first().click({ timeout: 30000 }).catch(() => {})
  await p.getByRole("alertdialog").getByRole("button", { name: "Close terminal" }).click({ timeout: 10000 }).catch(() => {})
  await browser.close()
}
process.exit(failed ? 1 : 0)
