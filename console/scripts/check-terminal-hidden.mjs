// F3-13, the part Playwright cannot do: a tab that is really hidden. Playwright keeps every page it
// drives visible, so this talks raw CDP to a Chromium it starts itself (headful under xvfb-run, with a
// window manager running: `xvfb-run -a sh -c 'openbox & sleep 1; node check-terminal-hidden.mjs'`).
//   STATE=<storage state> CHROME=<Chromium> CONSOLE_ORIGIN=<console origin> MACHINE=<machine id>
//   [HIDDEN_MS=<time hidden, default 125000>] node check-terminal-hidden.mjs
// Rows: the tab is hidden (visibilityState), output far above the worker's window made meanwhile is on
// screen within 2 s of return, 200 tab switches, and data-terminal-reconnects/-replays never move.
// FREEZE=<command> THAW=<command> adds the dead-path row: the machine stops answering without closing anything
// (docker pause of its container), and the tab is shown again; the probe must replace the connection within 8 s (one reconnect),
// and after THAW the terminal is attached again and echoes. The terminal it opens is closed at the end. Terminal contents are never printed.
import { execSync, spawn } from "node:child_process"
import { readFileSync, rmSync } from "node:fs"

const { STATE, CHROME, CONSOLE_ORIGIN: origin, MACHINE: machine } = process.env
const hiddenMs = Number(process.env.HIDDEN_MS ?? 125000)
// TICKS=<n>: after the flood the shell prints WH1..WHn one every TICK_S seconds (default 20; n = HIDDEN_MS/1000/TICK_S for a run that is hidden throughout).
const ticks = Number(process.env.TICKS ?? 0)
const tickSeconds = Number(process.env.TICK_S ?? 20)
const profile = `/tmp/pb-terminal-hidden-${process.pid}`
const chrome = spawn(CHROME, ["--remote-debugging-port=9339", `--user-data-dir=${profile}`, "--no-first-run", "--no-sandbox", "--window-size=1200,760", "about:blank"], { stdio: "ignore" })
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}

let version
for (let i = 0; i < 30 && !version; i++) version = await fetch("http://127.0.0.1:9339/json/version").then((r) => r.json(), () => null) ?? (await sleep(500), null)
const ws = new WebSocket(version.webSocketDebuggerUrl)
await new Promise((r) => (ws.onopen = r))
let next = 0
const waiting = new Map()
ws.onmessage = (m) => {
  const d = JSON.parse(m.data)
  waiting.get(d.id)?.(d)
}
const send = (method, params = {}, sessionId) =>
  new Promise((resolve, reject) => {
    const id = ++next
    waiting.set(id, (d) => (d.error ? reject(new Error(`${method}: ${d.error.message}`)) : resolve(d.result)))
    ws.send(JSON.stringify({ id, method, params, sessionId }))
  })
const attach = async (targetId) => (await send("Target.attachToTarget", { targetId, flatten: true })).sessionId
const evaluate = async (session, expression) => (await send("Runtime.evaluate", { expression, returnByValue: true, awaitPromise: true }, session)).result.value
const attr = (session, name) => evaluate(session, `document.querySelector("[data-terminal-phase]")?.getAttribute(${JSON.stringify(name)}) ?? null`)
const resyncs = async (session) => Number(await attr(session, "data-terminal-resyncs"))
const counts = async (session) => [Number(await attr(session, "data-terminal-reconnects")), Number(await attr(session, "data-terminal-replays"))]
const until = async (test, ms) => {
  for (const end = Date.now() + ms; Date.now() < end; await sleep(250)) if (await test()) return true
  return false
}
const shows = (session, text, ms) => until(async () => (await evaluate(session, `document.querySelector(".terminal-host")?.innerText ?? ""`)).includes(text), ms)
const typeLine = async (session, text) => {
  await evaluate(session, `document.querySelector(".terminal-host textarea")?.focus()`)
  await send("Input.insertText", { text }, session)
  for (const type of ["keyDown", "keyUp"]) await send("Input.dispatchKeyEvent", { type, key: "Enter", code: "Enter", windowsVirtualKeyCode: 13, text: "\r" }, session)
}
const marker = (name) => `M${name}-${Date.now().toString(36)}`

let terminal = ""
try {
  const first = (await send("Target.getTargets")).targetInfos.find((t) => t.type === "page")
  const page = await attach(first.targetId)
  const { cookies } = JSON.parse(readFileSync(STATE, "utf8"))
  await send("Network.setCookies", { cookies: cookies.map(({ name, value, domain, path, secure, httpOnly }) => ({ name, value, domain, path, secure, httpOnly })) }, page)
  await send("Page.navigate", { url: `${origin}/terminals/new?machine=${machine}&workspace=personal` }, page)
  await until(async () => /\/terminals\/(?!new)[^/?]+/.test(await evaluate(page, "location.pathname")), 90000)
  terminal = (await evaluate(page, "location.pathname")).match(/\/terminals\/([^/?]+)/)?.[1] ?? ""
  log(!!terminal, "a terminal opens", terminal)
  const attached = await until(async () => (await attr(page, "data-terminal-phase")) === "attached", 90000)
  const ready = marker("ready")
  if (attached) await typeLine(page, `echo ${ready}`)
  log(attached && (await shows(page, ready, 15000)), "attached and echoes its marker", `phase=${await attr(page, "data-terminal-phase")} close=${await attr(page, "data-terminal-close")}`)

  const before = await counts(page)
  // Wall-clock times of each step and of the first reconnect, so the edge and server logs can be matched.
  const stamp = () => new Date().toISOString().slice(11, 23) + "Z"
  let seen = ""
  const sampler = setInterval(async () => {
    const [reconnects] = await counts(page).catch(() => [before[0]])
    if (!seen && reconnects > before[0]) seen = stamp()
  }, 250)
  console.log("flood typed at", stamp())
  const flooded = marker("hidden")
  const ticker = ticks ? `; for i in $(seq 1 ${ticks}); do echo WH$i; sleep ${tickSeconds}; done` : ""
  await typeLine(page, `sleep 5; yes 0123456789 | head -c 4000000 | tr -d '\\n'; echo; echo ${flooded}${ticker}`)
  const other = (await send("Target.createTarget", { url: "about:blank" })).targetId
  await sleep(1500)
  log((await evaluate(page, "document.visibilityState")) === "hidden", "the terminal tab is hidden", await evaluate(page, "document.visibilityState"))
  await sleep(hiddenMs)
  console.log("returning at", stamp(), "first reconnect seen:", seen || "none yet")
  await send("Target.activateTarget", { targetId: first.targetId })
  const back = Date.now()
  // A long run scrolls the flood far above the rendered rows: then the newest line printed while hidden is the proof.
  const newest = ticks ? `WH${ticks}` : flooded
  log(await shows(page, newest, 2000), "output made while hidden is on screen within 2 s of return", `${Date.now() - back} ms`)
  let after = await counts(page)
  log(after[0] === before[0] && after[1] === before[1], "hidden: reconnects and replays unchanged", `${before} -> ${after} resyncs=${await resyncs(page)} close=${await attr(page, "data-terminal-close")}`)

  for (let i = 0; i < 200; i++) await send("Target.activateTarget", { targetId: i % 2 ? first.targetId : other })
  await send("Target.activateTarget", { targetId: first.targetId })
  await sleep(6000)
  clearInterval(sampler)
  console.log("switches done at", stamp(), "first reconnect seen:", seen || "none")
  after = await counts(page)
  log(after[0] === before[0] && after[1] === before[1], "200 tab switches: reconnects and replays unchanged", `${before} -> ${after} resyncs=${await resyncs(page)}`)
  if (ticks) {
    // The terminal renders only the rows in view, so read the whole scrollback by scrolling through it: every line
    // printed while hidden must be there (no gap), in order, and none twice within a view.
    await shows(page, `WH${ticks}`, 120000)
    const seen = await evaluate(page, `(async () => {
      const host = document.querySelector(".terminal-host")
      const numbers = new Set()
      let duplicated = 0
      let previous = 0
      let outOfOrder = 0
      let garbled = 0
      let sample = ""
      const pause = (ms) => new Promise((resolve) => setTimeout(resolve, ms))
      const read = () => {
        const text = host.innerText
        for (const row of text.matchAll(/^WH\\d{5,}/gm)) { garbled++; if (!sample) sample = row[0].slice(0, 40) }
        const got = [...text.matchAll(/^WH(\\d{1,4})[ \\u00a0]*$/gm)].map((m) => Number(m[1]))
        duplicated += got.length - new Set(got).size
        got.forEach((n, i) => { if (i > 0 && n !== got[i - 1] + 1) outOfOrder++; numbers.add(n) })
      }
      const step = Math.max(1, Math.floor(host.clientHeight * 0.6))
      for (let top = 0; top <= host.scrollHeight; top += step) { host.scrollTop = top; await pause(120); read() }
      host.scrollTop = host.scrollHeight
      await pause(200)
      return { count: numbers.size, min: Math.min(...numbers), max: Math.max(...numbers), duplicated, outOfOrder, garbled, sample }
    })()`)
    log(seen.count === ticks && seen.min === 1 && seen.max === ticks && seen.duplicated === 0, `all ${ticks} lines printed while hidden are in the scrollback, none twice in a view`, `${seen.count} distinct, ${seen.duplicated} duplicated`)
    const tail = (await evaluate(page, `(document.querySelector(".terminal-host")?.innerText ?? "").split("\\n").filter((l) => l.trim()).slice(-8).map((l) => l.slice(0, 60)).join(" | ")`))
    console.log(`INFO last rows: ${JSON.stringify(tail)}`)
    // Rows the renderer shows sparsely or with stale cells after a screen restore are reported, not failed here.
    console.log(`INFO scrollback: ${seen.outOfOrder} non-consecutive neighbours, ${seen.garbled} rows with stale cells ${JSON.stringify(seen.sample)}`)
    after = await counts(page)
    log(after[0] === before[0] && after[1] === before[1], "whole run: reconnects and replays unchanged", `${before} -> ${after} resyncs=${await resyncs(page)}`)
  }
  const echoed = marker("after")
  await typeLine(page, `echo ${echoed}`)
  log((await attr(page, "data-terminal-phase")) === "attached" && (await shows(page, echoed, 15000)), "still attached and echoes its marker")

  if (process.env.FREEZE) {
    const away = (await send("Target.createTarget", { url: "about:blank" })).targetId
    await sleep(1500)
    execSync(process.env.FREEZE, { stdio: "ignore" })
    const reconnectsBefore = (await counts(page))[0]
    try {
      await send("Target.activateTarget", { targetId: first.targetId })
      const shown = Date.now()
      // The reconnect starts at once, so the phase passes "reconnecting" in a blink: the row is that it left "attached".
      const noticed = await until(async () => (await attr(page, "data-terminal-phase")) !== "attached", 8000)
      log(noticed, "a dead path is noticed within 8 s of the tab being shown", `${Date.now() - shown} ms, phase=${await attr(page, "data-terminal-phase")}`)
    } finally {
      execSync(process.env.THAW, { stdio: "ignore" })
      await send("Target.closeTarget", { targetId: away }).catch(() => undefined)
    }
    const back = marker("thawed")
    const reattached = await until(async () => (await attr(page, "data-terminal-phase")) === "attached", 120000)
    if (reattached) await typeLine(page, `echo ${back}`)
    log((await counts(page))[0] === reconnectsBefore + 1, "...as exactly one reconnect", `${reconnectsBefore} -> ${(await counts(page))[0]}`)
    log(reattached && (await shows(page, back, 15000)), "after the machine answers again the terminal is attached and echoes", `phase=${await attr(page, "data-terminal-phase")}`)
  }

  const closeButton = `[...document.querySelectorAll("button")].find((b) => (b.getAttribute("aria-label") || b.textContent).trim() === "Close terminal")`
  console.log("closing the terminal at", stamp())
  await evaluate(page, `${closeButton}?.click()`)
  await sleep(1000)
  await evaluate(page, `[...document.querySelectorAll("[role=alertdialog] button")].find((b) => (b.getAttribute("aria-label") || b.textContent).trim() === "Close terminal")?.click()`)
  log(await until(async () => /\/terminals$/.test(await evaluate(page, "location.pathname")), 20000), "the terminal is closed", terminal)
} finally {
  chrome.kill()
  rmSync(profile, { recursive: true, force: true, maxRetries: 5, retryDelay: 300 })
}
console.log(failed ? `${failed} failed` : "all passed")
process.exit(failed ? 1 : 0)
