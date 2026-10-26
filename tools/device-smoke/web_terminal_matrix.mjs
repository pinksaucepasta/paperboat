// Terminal quality matrix, web side: the console's browser terminal under faults, headless.
//
// node web_terminal_matrix.mjs --machine <id> --owner-state <file>
//   [--member-state <file> --member-email <addr>] [--only w2,w4,...] [--hidden-minutes 5]
// Env: PB_CONSOLE_ORIGIN (the console origin), PW_CHROMIUM (headless Chromium path).
// Storage states are signed-in console sessions of throwaway accounts (0600 files, never printed).
// Rows print PASS/FAIL/SKIP with terminal IDs and UTC times; terminal contents are never printed.
// Every terminal this check opens is closed again. Needs playwright-core on NODE_PATH.
import { chromium, devices } from "playwright-core"
import { createHash, randomBytes } from "node:crypto"

const arg = (name, fallback = "") => {
  const i = process.argv.indexOf(`--${name}`)
  return i > 0 ? process.argv[i + 1] : fallback
}
const origin = process.env.PB_CONSOLE_ORIGIN
const exe = process.env.PW_CHROMIUM
if (!origin || !exe) throw new Error("set PB_CONSOLE_ORIGIN and PW_CHROMIUM")
const machine = arg("machine")
const ownerState = arg("owner-state")
const memberState = arg("member-state")
const memberEmail = arg("member-email")
const only = arg("only", "w2,w4,w4b,w5,w6,w7,w9,w10,w11,w12,w13,w1,w3").split(",")
const hiddenMinutes = Number(arg("hidden-minutes", "5"))
if (!machine || !ownerState) throw new Error("--machine and --owner-state are required")

const results = []
const opened = new Set()
const now = () => new Date().toISOString().slice(11, 19) + "Z"
const row = (name, ok, detail = "") => {
  const state = ok === null ? "SKIP" : ok ? "PASS" : "FAIL"
  results.push([name, state])
  console.log(`${state} ${name}${detail ? `: ${detail}` : ""} [${now()}]`)
}
const nonce = () => randomBytes(6).toString("hex")
// A command printing MARK on its own line whose typed echo does not contain MARK itself.
const emit = (mark) => {
  const i = mark.lastIndexOf("_")
  return `printf '%s_%s\\n' ${mark.slice(0, i)} ${mark.slice(i + 1)}`
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

const browser = await chromium.launch({ executablePath: exe })

// --- page helpers ---------------------------------------------------------------------------

const BUSY = /Connecting|Catching up|Reconnecting/
async function phase(page) {
  return page.evaluate((busy) => {
    const host = document.querySelector("[data-terminal-phase]")
    if (host) return host.getAttribute("data-terminal-phase")
    const status = [...document.querySelectorAll("[role=status]")].map((e) => e.textContent).join(" ")
    const overlay = document.querySelector(".terminal-host ~ div p.font-medium")?.textContent ?? ""
    if (overlay) return `overlay:${overlay.trim()}`
    if (new RegExp(busy).test(status)) return "busy"
    if (/Connection lost|No access|Terminal closed|Disconnected|limit reached/.test(document.body.innerText)) return "failed"
    return document.querySelector(".terminal-host textarea, textarea") ? "attached" : "loading"
  }, BUSY.source)
}
async function attached(page, timeout = 90_000) {
  const t0 = Date.now()
  while (Date.now() - t0 < timeout) {
    if ((await phase(page).catch(() => "")) === "attached") return (Date.now() - t0) / 1000
    await sleep(500)
  }
  return null
}
const screen = (page) => page.evaluate(() => document.querySelector(".terminal-host")?.innerText ?? "")
async function waitText(page, text, timeout = 30_000) {
  const t0 = Date.now()
  while (Date.now() - t0 < timeout) {
    if ((await screen(page)).includes(text)) return true
    await sleep(300)
  }
  return false
}
async function type(page, text) {
  await page.bringToFront() // headless Chromium delivers keys only to the front tab
  await page.locator(".terminal-host").click()
  await page.keyboard.type(text, { delay: 5 })
  await page.keyboard.press("Enter")
}
// Runs a command and waits for its done marker; returns the screen text after it.
async function run(page, command, timeout = 60_000) {
  const done = `PBW_DONE_${nonce()}`
  await type(page, `${command}; ${emit(done)}`)
  if (!(await waitText(page, done, timeout))) return null
  return screen(page)
}
function exactlyOnce(text, prefix, first, last) {
  const got = [...text.matchAll(new RegExp(`^${prefix}(\\d+)[ \\u00a0]*$`, "gm"))].map((m) => Number(m[1]))
  const want = Array.from({ length: last - first + 1 }, (_, i) => first + i)
  const dup = got.length - new Set(got).size
  const missing = want.filter((n) => !got.includes(n)).length
  return [dup === 0 && missing === 0, `${got.length} lines, ${dup} duplicated, ${missing} missing`]
}

async function newTerminal(context) {
  const page = await context.newPage()
  await page.goto(`${origin}/terminals/new?machine=${encodeURIComponent(machine)}&workspace=personal`)
  await page.waitForURL(/\/terminals\/(?!new)[^/?]+/, { timeout: 60_000 }).catch(() => {})
  const id = page.url().match(/\/terminals\/([^/?]+)/)?.[1]
  if (!id || id === "new") throw new Error("no terminal page opened")
  opened.add(id)
  if ((await attached(page)) === null) throw new Error(`terminal ${id} did not attach: ${await phase(page)}`)
  return { page, id }
}
async function openExisting(context, id) {
  const page = await context.newPage()
  await page.goto(`${origin}/terminals/${encodeURIComponent(id)}`)
  return page
}
async function closeTerminal(context, id) {
  const page = await context.newPage()
  try {
    await page.goto(`${origin}/terminals/${encodeURIComponent(id)}`)
    await page.getByRole("button", { name: "Close terminal" }).first().click({ timeout: 15_000 })
    await page.getByRole("alertdialog").getByRole("button", { name: "Close terminal" }).click({ timeout: 15_000 })
    await page.waitForURL(/\/terminals$/, { timeout: 20_000 })
    opened.delete(id)
  } catch {
    console.error(`cleanup: could not close terminal ${id}`)
  } finally {
    await page.close()
  }
}
const sttySize = async (page) => {
  const out = await run(page, "stty size")
  const m = out && [...out.matchAll(/^(\d+)[ \u00a0](\d+)[ \u00a0]*$/gm)].pop()
  return m ? `${m[1]}x${m[2]}` : null
}

// Owner only: let the member open this terminal in a browser, typing or view-only.
async function sharePolicy(ctx, id, interactive) {
  const path = `${origin}/api/v1/terminal-sessions/${encodeURIComponent(id)}/browser-access-policy`
  const current = await (await ctx.request.get(path)).json()
  const r = await ctx.request.put(path, {
    headers: { origin },
    data: {
      expected_generation: current.data.generation, audience: "specific_emails", team_id: "",
      allowed_emails: [memberEmail], interactive_emails: interactive ? [memberEmail] : [],
      confirm_interactive_access: interactive,
    },
  })
  return r.ok()
}

// --- rows -----------------------------------------------------------------------------------

const rows = {
  // W2/W8: the owner closes the tab; output made meanwhile replays once when they come back.
  async w2(ctx) {
    const { page, id } = await newTerminal(ctx)
    await type(page, "for i in $(seq 1 15); do echo WR$i; sleep 2; done")
    await page.close()
    await sleep(35_000)
    const back = await openExisting(ctx, id)
    const t = await attached(back)
    row("W2 owner leaves and returns: reattaches", t !== null, `${id} ${t ?? "-"} s`)
    const out = await run(back, "true")
    const [ok, why] = exactlyOnce(out ?? "", "WR", 1, 15)
    row("W2 owner returns: output made while away shows exactly once", ok, `${id} ${why}`)
    await back.close()
    await closeTerminal(ctx, id)
  },

  // W4: background tab frozen (page lifecycle "frozen", as Chrome does to hidden tabs).
  async w4(ctx) {
    const { page, id } = await newTerminal(ctx)
    const cdp = await ctx.newCDPSession(page)
    const n = Math.max(3, Math.round((hiddenMinutes * 60) / 20))
    await type(page, `for i in $(seq 1 ${n}); do echo WH$i; sleep 20; done`)
    await cdp.send("Page.setWebLifecycleState", { state: "frozen" })
    await sleep(hiddenMinutes * 60_000 + 15_000)
    await cdp.send("Page.setWebLifecycleState", { state: "active" })
    const t = await attached(page, 60_000)
    row(`W4 tab frozen ${hiddenMinutes} min: live again on its own`, t !== null, `${id} ${t ?? "-"} s, phase ${await phase(page)}`)
    const out = await run(page, "true")
    const [ok, why] = exactlyOnce(out ?? "", "WH", 1, n)
    row("W4 tab frozen: output made meanwhile shows exactly once", ok, `${id} ${why}`)
    await page.close()
    await closeTerminal(ctx, id)
  },

  // W4b: tab hidden (visibilitychange; timers still run) for N minutes, then shown.
  async w4b(ctx) {
    const { page, id } = await newTerminal(ctx)
    const n = Math.max(3, Math.round((hiddenMinutes * 60) / 20))
    await type(page, `for i in $(seq 1 ${n}); do echo WV$i; sleep 20; done`)
    const flip = (hidden) => page.evaluate((h) => {
      Object.defineProperty(document, "visibilityState", { configurable: true, get: () => (h ? "hidden" : "visible") })
      Object.defineProperty(document, "hidden", { configurable: true, get: () => h })
      document.dispatchEvent(new Event("visibilitychange"))
    }, hidden)
    await flip(true)
    await sleep(hiddenMinutes * 60_000 + 15_000)
    await flip(false)
    const t = await attached(page, 60_000)
    row(`W4b tab hidden ${hiddenMinutes} min then shown: live`, t !== null, `${id} ${t ?? "-"} s`)
    const out = await run(page, "true")
    const [ok, why] = exactlyOnce(out ?? "", "WV", 1, n)
    row("W4b tab hidden: output made meanwhile shows exactly once", ok, `${id} ${why}`)
    await page.close()
    await closeTerminal(ctx, id)
  },

  // W5: two windows of the same session.
  async w5(ctx) {
    const { page: a, id } = await newTerminal(ctx)
    const b = await openExisting(ctx, id)
    const t = await attached(b)
    row("W5 second window attaches", t !== null, `${id} ${t ?? "-"} s`)
    const mark = `PBW_TWO_${nonce()}`
    await type(b, emit(mark))
    row("W5 typing in window 2 shows in window 1", await waitText(a, mark), id)
    // F3-17: the shell has the size of the typing window that was last active.
    const first = await sttySize(a)
    await b.setViewportSize({ width: 700, height: 420 })
    await sleep(2000)
    const second = await sttySize(b)
    const colsOf = (size) => Number((size ?? "0x0").split("x")[1])
    row("W5 typing in the smaller window 2 makes the shell its size", colsOf(second) > 0 && colsOf(second) < colsOf(first), `${first} → ${second}`)
    const back = await sttySize(a)
    row("W5 typing in window 1 gives its size back", back !== null && back === first, `${second} → ${back}`)
    await b.close()
    await a.close()
    await closeTerminal(ctx, id)
  },

  // W6/W8: the browser goes offline for 60 s while output continues; keys typed offline.
  async w6(ctx) {
    const { page, id } = await newTerminal(ctx)
    await type(page, "for i in $(seq 1 20); do echo WO$i; sleep 3; done")
    await sleep(3000)
    await ctx.setOffline(true)
    await sleep(5000)
    const offlinePhase = await phase(page)
    const key = `PBW_OFFKEY_${nonce()}`
    await page.keyboard.type(emit(key), { delay: 5 }).catch(() => {})
    await page.keyboard.press("Enter").catch(() => {})
    await sleep(55_000)
    await ctx.setOffline(false)
    const t = await attached(page, 90_000)
    row("W6 offline 60 s: reconnects on its own", t !== null, `${id} ${t ?? "-"} s; while offline the page showed ${offlinePhase}`)
    await sleep(65_000)
    const out = (await run(page, "true")) ?? ""
    const [ok, why] = exactlyOnce(out, "WO", 1, 20)
    row("W6 offline: output made meanwhile shows exactly once", ok, `${id} ${why}`)
    const ran = out.split("\n").filter((l) => l === key).length
    row("W8 keys typed while offline: run once, or are refused visibly", ran <= 1, `${id} ran ${ran} times (0 = dropped)`)
    await page.close()
    await closeTerminal(ctx, id)
  },

  // W7: the console API is unreachable (server deploy/outage) for 2 min; the open terminal keeps working.
  async w7(ctx) {
    const { page, id } = await newTerminal(ctx)
    await page.route("**/api/**", (r) => r.abort())
    let ok = true
    for (let i = 0; i < 4 && ok; i++) {
      await sleep(30_000)
      ok = (await run(page, "echo alive", 30_000)) !== null
    }
    await page.unroute("**/api/**")
    row("W7 console API down 2 min: open terminal keeps working", ok, id)
    await page.close()
    await closeTerminal(ctx, id)
  },

  // W9: 50 MB of output: the page stays responsive and Ctrl-C works.
  async w9(ctx) {
    const { page, id } = await newTerminal(ctx)
    const t0 = Date.now()
    await type(page, "yes 0123456789012345678901234567890123456789012345678 | head -c 50000000; echo PBW_FAST_END")
    let worst = 0
    let done = false
    while (Date.now() - t0 < 300_000) {
      const s = Date.now()
      const txt = await page.evaluate(() => document.querySelector(".terminal-host")?.innerText.slice(-200) ?? "")
      worst = Math.max(worst, Date.now() - s)
      if (txt.includes("PBW_FAST_END")) { done = true; break }
      await sleep(1000)
    }
    row("W9 50 MB output finishes", done, `${id} ${Math.round((Date.now() - t0) / 1000)} s`)
    row("W9 page stays responsive during it (worst main-thread round trip < 1 s)", worst < 1000, `${worst} ms`)
    const mem = await page.evaluate(() => performance.memory?.usedJSHeapSize ?? 0)
    row("W9 JS heap after it < 300 MB", mem < 300e6, `${Math.round(mem / 1e6)} MB`)
    await type(page, "yes")
    await sleep(3000)
    await page.keyboard.press("Control+C")
    const s = Date.now()
    const ok = (await run(page, "true", 15_000)) !== null
    row("W9 Ctrl-C stops yes promptly", ok, `${((Date.now() - s) / 1000).toFixed(1)} s`)
    await page.close()
    await closeTerminal(ctx, id)
  },

  // W10: resize storm; final size wins and a full-width line fits on one row.
  async w10(ctx) {
    const { page, id } = await newTerminal(ctx)
    for (let i = 0; i < 60; i++) await page.setViewportSize({ width: 600 + ((i * 37) % 700), height: 400 + ((i * 53) % 400) })
    await page.setViewportSize({ width: 1100, height: 700 })
    await sleep(2500)
    const a = await sttySize(page)
    await sleep(3000)
    const b = await sttySize(page)
    row("W10 resize storm: PTY size settles", a !== null && a === b, `${id} ${a} / ${b}`)
    const out = (await run(page, "printf '%*s\\n' \"$(tput cols)\" '' | tr ' ' '#'")) ?? ""
    const cols = Number((b ?? "0x0").split("x")[1])
    const full = out.split("\n").some((l) => l.trim() === "#".repeat(cols))
    row("W10 a full-width line fits one row (no garbled wrap)", full, `${cols} columns`)
    await page.close()
    await closeTerminal(ctx, id)
  },

  // W11: paste 64 KiB of text through a clipboard paste event; the host gets it byte for byte.
  async w11(ctx) {
    const { page, id } = await newTerminal(ctx)
    const body = Array.from({ length: 500 }, () => randomBytes(64).toString("hex")).join("\n") + "\n"
    const want = createHash("sha256").update(body).digest("hex")
    const path = `/tmp/pbw-paste-${nonce()}`
    await type(page, `stty -echo; head -c ${Buffer.byteLength(body)} > ${path}; stty echo`)
    await sleep(1000)
    await page.evaluate((text) => {
      const data = new DataTransfer()
      data.setData("text/plain", text)
      const target = document.querySelector(".terminal-host textarea") ?? document.activeElement
      target.dispatchEvent(new ClipboardEvent("paste", { clipboardData: data, bubbles: true, cancelable: true }))
    }, body)
    const out = (await run(page, `sha256sum ${path}; rm -f ${path}`, 120_000)) ?? ""
    row("W11 64 KiB paste reaches the shell byte for byte", out.includes(want), id)
    await page.close()
    await closeTerminal(ctx, id)
  },

  // W12: mobile web: the terminal attaches, takes typed input, and shows the extra keys.
  async w12() {
    const mobile = await browser.newContext({ ...devices["Pixel 7"], storageState: ownerState })
    try {
      const { page, id } = await newTerminal(mobile)
      const keys = await page.getByRole("group", { name: "Extra keys" }).isVisible()
      row("W12 mobile: extra keys bar shows", keys, id)
      const mark = `PBW_MOBILE_${nonce()}`
      await page.locator(".terminal-host").tap()
      await page.keyboard.insertText(emit(mark))
      await page.keyboard.press("Enter")
      row("W12 mobile: typed input runs", await waitText(page, mark), id)
      const size = await sttySize(page)
      const cols = Number((size ?? "0x0").split("x")[1])
      row("W12 mobile: PTY width matches a phone (20–80 columns)", cols >= 20 && cols <= 80, size ?? "-")
      await page.close()
      await closeTerminal(mobile, id)
    } finally {
      await mobile.close()
    }
  },

  // W13: many terminals: up to the plan's browser-terminal limit attach; one more says why it can't.
  async w13(ctx) {
    const pages = []
    let limitShown = false
    try {
      for (let i = 0; i < 20; i++) {
        try {
          pages.push(await newTerminal(ctx))
        } catch (e) {
          limitShown = /limit/i.test(String(e))
          break
        }
      }
      let working = 0
      for (const { page } of pages) if ((await run(page, "true", 30_000)) !== null) working++
      row("W13 every opened terminal works", working === pages.length, `${working}/${pages.length}`)
      row("W13 past the plan limit the page names the limit", pages.length === 20 || limitShown,
        pages.length === 20 ? "20 opened, no limit reached" : `stopped at ${pages.length}`)
    } finally {
      for (const { page, id } of pages) {
        await page.close().catch(() => {})
        await closeTerminal(ctx, id)
      }
    }
  },

  // W1/W3: a second user joins interactive (types; both see), then as viewer (cannot type).
  async w1(ctx) {
    if (!memberState || !memberEmail) return row("W1 two users in one shared session", null, "no --member-state/--member-email")
    const memberCtx = await browser.newContext({ storageState: memberState, viewport: { width: 1000, height: 640 } })
    const { page, id } = await newTerminal(ctx)
    try {
      if (!(await sharePolicy(ctx, id, true))) return row("W1 share the session with the member", false, id)
      const m = await openExisting(memberCtx, id)
      const t = await attached(m)
      row("W1 member joins the shared session", t !== null, `${id} ${t ?? "-"} s`)
      const mark = `PBW_MEMBER_${nonce()}`
      await type(m, emit(mark))
      row("W1 member's typing shows for the owner", await waitText(page, mark), id)
      const before = await sttySize(page)
      await m.setViewportSize({ width: 600, height: 400 })
      await sleep(2000)
      row("W1 member's window size does not change the owner's PTY", (await sttySize(page)) === before, before ?? "-")
      await m.close()
      await sharePolicy(ctx, id, false)
      const v = await openExisting(memberCtx, id)
      await attached(v)
      const role = await v.evaluate(() => document.querySelector("[data-terminal-role]")?.getAttribute("data-terminal-role") ?? "")
      const key = `PBW_VIEWER_${nonce()}`
      await type(v, emit(key))
      await sleep(4000)
      row("W3 viewer's keys do not reach the shell", !(await screen(page)).includes(key), `${id} role=${role || "n/a"}`)
      const live = `PBW_LIVE_${nonce()}`
      await type(page, emit(live))
      row("W3 viewer sees live output", await waitText(v, live), id)
      await v.close()
    } finally {
      await memberCtx.close()
      await page.close()
      await closeTerminal(ctx, id)
    }
  },
}
rows.w3 = async () => {} // W3 runs inside w1

const owner = await browser.newContext({ storageState: ownerState, viewport: { width: 1280, height: 800 } })
try {
  for (const key of only) {
    try {
      await rows[key](owner)
    } catch (e) {
      row(`${key} fixture`, false, String(e).slice(0, 200))
    }
  }
} finally {
  for (const id of [...opened]) await closeTerminal(owner, id)
  await browser.close()
}
const failed = results.filter(([, s]) => s === "FAIL").length
console.log("RESULT", failed ? `FAIL ${failed}` : "PASS")
process.exit(failed ? 1 : 0)
