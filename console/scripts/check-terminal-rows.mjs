// Headless checks for the browser terminal rows F3-13 (hidden tab), F3-14 (two windows) and F3-19
// (truthful errors) on a Linux machine. Run with playwright-core installed:
//   STATE=<storage state> CHROME=<Chromium> CONSOLE_ORIGIN=<console origin> MACHINE=<machine id>
//   ROW=<f307|f308|f313|f314|f319|w15>  (f308 also needs MEMBER_STATE and MEMBER_EMAIL: a second console account) [CHROME_ARG=<one extra Chromium flag>] [HIDDEN_MS=<frozen time, default 125000>] node check-terminal-rows.mjs
// A row passes only when data-terminal-phase is "attached" and an echoed marker shows on screen.
// Screenshots: /tmp/terminal-rows-<row>-*.png. The terminal it opens is closed at the end.
import { chromium } from "playwright-core"
import { execSync } from "node:child_process"

const { STATE, CHROME, CONSOLE_ORIGIN: origin, MACHINE: machine, ROW: row, CHROME_ARG } = process.env
// w15 cuts the machine off and restores it with these shell commands (a firewall rule on the machine).
const { MACHINE_BLOCK, MACHINE_UNBLOCK, MEMBER_STATE, MEMBER_EMAIL } = process.env
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
const resyncs = async (p) => Number(await attr(p, "data-terminal-resyncs"))
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
  log(echoed, `${name}: attached and echoes its marker`, `phase=${await attr(p, "data-terminal-phase")} close=${await attr(p, "data-terminal-close")}`)
  return echoed
}

const flood = (m) => `yes 0123456789 | head -c 4000000 | tr -d '\\n'; echo; echo ${m}`
const stamp = () => new Date().toISOString().slice(11, 19) + "Z"

// 200 tab and window switches cost no reconnect and no replay; only a real freeze (Page Lifecycle) may
// restore from a checkpoint. A really hidden tab is check-terminal-hidden.mjs (Playwright cannot hide one).
async function f313() {
  const { p, id } = await open()
  if (!(await live(p, "before"))) return id
  const other = await ctx.newPage()
  await other.goto("about:blank")
  let before = await counts(p)
  let after

  // 200 tab and window switches.
  for (let i = 0; i < 200; i++) {
    const [front, rest] = i % 2 ? [p, other] : [other, p]
    await front.bringToFront()
    await rest.evaluate(() => window.dispatchEvent(new Event("blur")))
    await front.evaluate(() => window.dispatchEvent(new Event("focus")))
  }
  await p.bringToFront()
  await p.waitForTimeout(6000)
  after = await counts(p)
  log(after[0] === before[0] && after[1] === before[1], "200 switches: reconnects and replays unchanged", `${before} -> ${after}`)
  await live(p, "after-switches")

  // Frozen: the page and its worker stop; on resume the screen is restored (a checkpoint, one paint).
  before = await counts(p)
  const frozenMarker = marker("frozen")
  await type(p, `sleep 5; ${flood(frozenMarker)}`)
  const cdp = await ctx.newCDPSession(p)
  console.log("frozen at", stamp())
  await cdp.send("Page.setWebLifecycleState", { state: "frozen" })
  await p.waitForTimeout(hiddenMs)
  await cdp.send("Page.setWebLifecycleState", { state: "active" })
  console.log("resumed at", stamp())
  log(await phaseIs(p, "attached", 30000), "frozen: attached again on its own")
  log(await shows(p, frozenMarker, 15000), "frozen: output made meanwhile is on screen")
  after = await counts(p)
  log(after[1] === before[1], "frozen: no replayed output", `${before} -> ${after}`)
  console.log(`INFO frozen: reconnects ${before[0]} -> ${after[0]}, resyncs=${await resyncs(p)}, close=${await attr(p, "data-terminal-close")}`)
  await live(p, "after-freeze")
  await other.close()
  return id
}

// W15: a drained carrier says "machine unavailable" within 5 s (never "No access") and recovers by itself.
async function w15() {
  const { p, id } = await open()
  if (!(await live(p, "before"))) return id
  const run = (command) => execSync(command, { timeout: 60000 })
  let extra = ""
  try {
    run(MACHINE_BLOCK)
    await p.waitForTimeout(60000)
    const q = await ctx.newPage()
    const start = Date.now()
    await q.goto(`${origin}/terminals/new?machine=${machine}&workspace=personal`, { timeout: 280000 })
    await q.waitForURL(/\/terminals\/(?!new)[^/?]+/, { timeout: 60000 }).catch(() => {})
    const second = q.url().match(/\/terminals\/([^/?]+)/)?.[1]
    const said = await q
      .waitForFunction(() => /machine is unavailable/i.test(document.querySelector("[data-terminal-phase]")?.innerText ?? ""), null, { timeout: 15000 })
      .then(() => (Date.now() - start) / 1000, () => null)
    const text = (await q.evaluate(() => document.querySelector("[data-terminal-phase]")?.innerText ?? "")).replace(/\s+/g, " ")
    log(said !== null && said < 5 + 2, "a drained machine says 'machine unavailable' within 5 s of the page opening", `${said ?? "-"} s`)
    log(!/No access/i.test(text), "and never says 'No access'", text.slice(0, 100))
    run(MACHINE_UNBLOCK)
    const back = Date.now()
    log(await phaseIs(q, "attached", 180000), "it attaches by itself when the machine returns", `${((Date.now() - back) / 1000).toFixed(0)} s`)
    extra = second
  } finally {
    run(MACHINE_UNBLOCK) // never leave the machine cut off
  }
  return [id, extra]
}

const screenText = (p) => p.evaluate(() => document.querySelector(".terminal-host")?.innerText ?? "")
const sttyCols = async (p) => {
  const m = marker("size")
  await type(p, `printf '${m}%s\\n' "$(stty size)"`)
  await shows(p, m, 15000)
  return Number([...(await screenText(p)).matchAll(new RegExp(`${m}(\\d+)[ \\u00a0](\\d+)`, "g"))].pop()?.[2])
}

// F3-07: open, type, resize, reconnect, close, from the console.
async function f307() {
  const { p, id } = await open()
  if (!(await live(p, "open+type"))) return id
  const wide = await sttyCols(p)
  await p.setViewportSize({ width: 800, height: 520 })
  await p.waitForTimeout(3000)
  const narrow = await sttyCols(p)
  log(wide > 0 && narrow > 0 && narrow < wide, "resize: the shell's width follows the window", `${wide} -> ${narrow} columns`)
  const kept = marker("kept")
  await type(p, `echo ${kept}`)
  await shows(p, kept, 15000)
  await p.reload({ waitUntil: "domcontentloaded" })
  log(await phaseIs(p, "attached"), "reconnect: a reload attaches again")
  log(await shows(p, kept, 10000), "reconnect: the earlier output is on screen again")
  await live(p, "after-reload")
  await p.getByRole("button", { name: "Close terminal" }).first().click({ timeout: 30000 })
  await p.getByRole("alertdialog").getByRole("button", { name: "Close terminal" }).click({ timeout: 10000 })
  log(await p.waitForURL(/\/terminals$/, { timeout: 20000 }).then(() => true, () => false), "close: the console returns to the list")
  await p.goto(`${origin}/terminals/${id}`, { timeout: 120000 })
  log(await phaseIs(p, "closed", 30000), "close: the terminal reads closed", await attr(p, "data-terminal-phase"))
  return []
}

const people = (p) => p.evaluate(() => [...document.querySelectorAll("aside[aria-label=People] li")].map((li) => li.innerText.replace(/\s+/g, " ").trim()))
const peopleHave = (p, text, ms = 15000) =>
  p.waitForFunction((t) => [...document.querySelectorAll("aside[aria-label=People] li")].some((li) => li.innerText.includes(t)), text, { timeout: ms }).then(() => true, () => false)
const peopleLack = (p, text, ms = 15000) =>
  p.waitForFunction((t) => ![...document.querySelectorAll("aside[aria-label=People] li")].some((li) => li.innerText.includes(t)), text, { timeout: ms }).then(() => true, () => false)

// F3-08: the sharing panel: real identities, join codes, guests with and without an account, presence.
async function f308() {
  const { p, id } = await open()
  if (!(await live(p, "owner"))) return id
  await p.getByRole("button", { name: /^People/ }).click({ timeout: 15000 }).catch(() => {})
  log(await peopleHave(p, "Owner"), "the panel lists the owner", (await people(p)).join(" | "))
  await p.getByRole("button", { name: "Share", exact: true }).click({ timeout: 15000 })
  const code = await p
    .waitForFunction(() => document.querySelector("[role=dialog] span.font-mono.text-lg")?.textContent?.trim() || null, null, { timeout: 30000 })
    .then((h) => h.jsonValue(), () => null)
  log(!!code, "Share makes a join code", code ? "code shown" : "no code")
  await p.keyboard.press("Escape")

  // A guest with no account: the code and a name, in a browser with nothing signed in.
  const guestCtx = await browser.newContext({ viewport: { width: 1100, height: 700 } })
  const guest = await guestCtx.newPage()
  await guest.goto(`${origin}/terminal/${id}`, { timeout: 120000 })
  await guest.getByText("Enter it", { exact: true }).click({ timeout: 30000 })
  await guest.locator("#join-code").fill(code ?? "")
  await guest.locator("#join-name").fill("Guest Tester")
  await guest.getByRole("button", { name: "Join", exact: true }).click()
  log(await phaseIs(guest, "attached"), "a guest with no account joins with the code")
  log(await peopleHave(p, "Guest Tester"), "the owner's panel lists the guest by name", (await people(p)).join(" | "))
  log(await peopleHave(p, "Guest"), "...marked as a guest (no account)")
  const watch = marker("guestkey")
  await type(guest, `echo ${watch}`).catch(() => {})
  await p.waitForTimeout(2500)
  log(!(await screenText(p)).includes(watch), "a code guest is watch-only: their keys do not reach the shell")
  await guest.close()
  log(await peopleLack(p, "Guest Tester", 15000), "presence: the guest leaves the panel when their tab closes")
  await guestCtx.close()

  // A signed-in teammate: shared by email, listed by their real name, not an id.
  if (MEMBER_STATE && MEMBER_EMAIL) {
    const path = `${origin}/api/v1/terminal-sessions/${encodeURIComponent(id)}/browser-access-policy`
    const current = await (await ctx.request.get(path)).json()
    const shared = await ctx.request.put(path, {
      headers: { origin },
      data: { expected_generation: current.data.generation, audience: "specific_emails", team_id: "", allowed_emails: [MEMBER_EMAIL], interactive_emails: [], confirm_interactive_access: false },
    })
    log(shared.ok(), "the owner shares the terminal with the teammate's email")
    const memberCtx = await browser.newContext({ storageState: MEMBER_STATE, viewport: { width: 1100, height: 700 } })
    const member = await memberCtx.newPage()
    await member.goto(`${origin}/terminals/${id}`, { timeout: 120000 })
    log(await phaseIs(member, "attached"), "the teammate joins from their own account")
    log(await peopleHave(p, "Lane Terminal"), "the owner's panel lists the teammate by real name, not an id", (await people(p)).join(" | "))
    await member.close()
    log(await peopleLack(p, "Lane Terminal", 15000), "presence: the teammate leaves the panel when their tab closes")
    await memberCtx.close()
  } else console.log("SKIP  teammate steps: set MEMBER_STATE and MEMBER_EMAIL")
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
  // The size rule (F3-17): the shell has the size of the typing window that was last active.
  await b.setViewportSize({ width: 800, height: 520 })
  await a.bringToFront()
  await a.waitForTimeout(3000)
  const wide = await sttyCols(a)
  await b.bringToFront()
  const narrow = await sttyCols(b)
  log(wide > 0 && narrow > 0 && narrow < wide, "size: typing in the smaller window makes the shell its size", `${wide} -> ${narrow} columns`)
  await a.bringToFront()
  const back = await sttyCols(a)
  log(back === wide, "size: typing in the first window gives the size back", `${narrow} -> ${back} columns`)
  const phases = [await attr(a, "data-terminal-phase"), await attr(b, "data-terminal-phase")]
  log(phases.every((phase) => phase === "attached"), "both windows stay attached", phases.join(" "))
  const moved = [...(await counts(a)), ...(await counts(b))]
  log(moved[0] === 0 && moved[2] === 0, "switching between the windows caused no reconnect", `a ${moved.slice(0, 2)} b ${moved.slice(2)}`)
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

for (const id of [await { f307, f308, f313, f314, f319, w15 }[row]()].flat()) {
  if (!id || id === "new") continue
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
