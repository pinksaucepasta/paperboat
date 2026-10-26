// Headless checks for the browser terminal rows F3-13 (hidden tab), F3-14 (two windows) and F3-19
// (truthful errors) on a Linux machine. Run with playwright-core installed:
//   STATE=<storage state> CHROME=<Chromium> CONSOLE_ORIGIN=<console origin> MACHINE=<machine id>
//   ROW=<f313|f314|f319> [CHROME_ARG=<one extra Chromium flag>] [HIDDEN_MS=<frozen time, default 125000>] node check-terminal-rows.mjs
// A row passes only when data-terminal-phase is "attached" and an echoed marker shows on screen.
// Screenshots: /tmp/terminal-rows-<row>-*.png. The terminal it opens is closed at the end.
import { chromium } from "playwright-core"

const { STATE, CHROME, CONSOLE_ORIGIN: origin, MACHINE: machine, ROW: row, CHROME_ARG } = process.env
const hiddenMs = Number(process.env.HIDDEN_MS ?? 125000)
const browser = await chromium.launch({ executablePath: CHROME, args: CHROME_ARG ? [CHROME_ARG] : [] })
const ctx = await browser.newContext({ storageState: STATE, ignoreHTTPSErrors: true, viewport: { width: 1200, height: 760 } })
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const attr = (p, name) => p.evaluate((n) => document.querySelector("[data-terminal-phase]")?.getAttribute(n) ?? null, name)
const phaseIs = (p, want, ms = 90000) =>
  p
    .waitForFunction((w) => document.querySelector("[data-terminal-phase]")?.getAttribute("data-terminal-phase") === w, want, { timeout: ms })
    .then(() => true, () => false)
const shows = (p, text, ms = 15000) =>
  p
    .waitForFunction((t) => (document.querySelector(".terminal-host")?.innerText ?? "").includes(t), text, { timeout: ms })
    .then(() => true, () => false)
const type = async (p, text) => {
  await p.locator(".terminal-host").click()
  await p.keyboard.type(text, { delay: 15 })
  await p.keyboard.press("Enter")
}
const shot = (p, name) => p.screenshot({ path: `/tmp/terminal-rows-${row}-${name}.png` })
const marker = (name) => `M${name}-${Date.now().toString(36)}`
const counts = async (p) => [Number(await attr(p, "data-terminal-reconnects")), Number(await attr(p, "data-terminal-replays"))]

async function open() {
  const p = await ctx.newPage()
  await p.goto(`${origin}/terminals/new?machine=${machine}&workspace=personal`, { timeout: 280000 })
  await p.waitForURL(/\/terminals\/(?!new)[^/?]+/, { timeout: 60000 }).catch(() => {})
  const id = p.url().match(/\/terminals\/([^/?]+)/)?.[1]
  log(!!id && id !== "new", "a terminal opens", id)
  return { p, id }
}

/** The pass rule: attached, and a fresh marker typed now is echoed. */
async function live(p, name) {
  const attached = await phaseIs(p, "attached")
  const m = marker(name)
  if (attached) await type(p, `echo ${m}`)
  const echoed = attached && (await shows(p, m))
  await shot(p, name)
  log(echoed, `${name}: attached and echoes its marker`, `phase=${await attr(p, "data-terminal-phase")}`)
  return echoed
}

async function f313() {
  const { p, id } = await open()
  if (!(await live(p, "before"))) return id
  const before = await counts(p)
  // Printed while the tab is hidden and frozen; it must be on screen on return without a replay.
  const hiddenMarker = marker("hidden")
  await type(p, `sleep 30; yes 0123456789 | head -c 4000000 | tr -d '\\n'; echo; echo ${hiddenMarker}`)
  const other = await ctx.newPage()
  await other.goto("about:blank")
  await other.bringToFront()
  await p.waitForTimeout(1500)
  const cdp = await ctx.newCDPSession(p)
  await cdp.send("Page.setWebLifecycleState", { state: "frozen" })
  await p.waitForTimeout(hiddenMs)
  await cdp.send("Page.setWebLifecycleState", { state: "active" })
  for (let i = 0; i < 200; i++) {
    const [front, back] = i % 2 ? [p, other] : [other, p]
    await front.bringToFront()
    await back.evaluate(() => window.dispatchEvent(new Event("blur")))
    await front.evaluate(() => {
      document.dispatchEvent(new Event("visibilitychange"))
      window.dispatchEvent(new Event("focus"))
    })
  }
  await p.bringToFront()
  await p.waitForTimeout(6000)
  const after = await counts(p)
  log(after[0] === before[0] && after[1] === before[1], "reconnects and replays unchanged", `${before} -> ${after}`)
  log(await shows(p, hiddenMarker, 5000), "output printed while hidden is on screen")
  await live(p, "after")
  await other.close()
  return id
}

async function f314() {
  const { p: a, id } = await open()
  if (!(await live(a, "a"))) return id
  const b = await ctx.newPage()
  await b.goto(`${origin}/terminals/${id}`, { timeout: 280000 })
  log(await phaseIs(b, "attached"), "the second window attaches")
  for (const [from, to, name] of [[a, b, "from-a"], [b, a, "from-b"]]) {
    const m = marker(name)
    await from.bringToFront()
    await type(from, `echo ${m}`)
    const both = (await shows(from, m)) && (await shows(to, m))
    await shot(from, `${name}-typed`)
    await shot(to, `${name}-other`)
    log(both, `${name}: both windows echo`)
  }
  const phases = [await attr(a, "data-terminal-phase"), await attr(b, "data-terminal-phase")]
  log(phases.every((phase) => phase === "attached"), "both windows stay attached", phases.join(" "))
  await b.close()
  return id
}

async function f319() {
  const { p, id } = await open()
  if (!(await live(p, "real"))) return id
  // Each typed ticket refusal, answered in a fresh window of the same terminal, shows its own words.
  const refusals = [
    [403, "forbidden", "denied", /do not have access/],
    [404, "not_found", "closed", /ended/],
    [409, "machine_edge_unavailable", "reconnecting", /machine is unavailable/],
    [409, "terminal_session_closed", "closed", /ended/],
    [409, "browser_terminal_limit_reached", "limit", /limit/i],
  ]
  for (const [status, code, phase, words] of refusals) {
    const q = await ctx.newPage()
    await q.route("**/browser-ticket", (r) =>
      r.fulfill({ status, contentType: "application/json", body: JSON.stringify({ error: { code, message: "stub" } }) }),
    )
    await q.goto(`${origin}/terminals/${id}`, { timeout: 280000 })
    await phaseIs(q, phase, 30000)
    const text = (await q.evaluate(() => document.querySelector("[data-terminal-phase]")?.innerText ?? "")).replace(/\s+/g, " ")
    await shot(q, `${status}-${code}`)
    const got = await attr(q, "data-terminal-phase")
    const ok = got === phase && words.test(text) && !(status === 404 && /No access/i.test(text))
    log(ok, `${status} ${code}: ${phase} with its own message`, `${got}: ${text.slice(0, 120)}`)
    await q.close()
  }
  return id
}

const id = await { f313, f314, f319 }[row]()
if (id && id !== "new") {
  const p = await ctx.newPage()
  await p.goto(`${origin}/terminals/${id}`, { timeout: 280000 })
  await p.getByRole("button", { name: "Close terminal" }).first().click({ timeout: 30000 }).catch(() => {})
  await p.getByRole("alertdialog").getByRole("button", { name: "Close terminal" }).click({ timeout: 10000 }).catch(() => {})
  const closed = await p.waitForURL(/\/terminals$/, { timeout: 20000 }).then(() => true, () => false)
  log(closed, "the terminal is closed", id)
}
await browser.close()
console.log(failed ? `${failed} failed` : "all passed")
process.exit(failed ? 1 : 0)
