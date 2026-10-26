// F3-13, the part Playwright cannot do: a tab that is really hidden. Playwright keeps every page it
// drives visible, so this talks raw CDP to a Chromium it starts itself (headful under xvfb-run, with a
// window manager running: `xvfb-run -a sh -c 'openbox & sleep 1; node check-terminal-hidden.mjs'`).
//   STATE=<storage state> CHROME=<Chromium> CONSOLE_ORIGIN=<console origin> MACHINE=<machine id>
//   [HIDDEN_MS=<time hidden, default 125000>] node check-terminal-hidden.mjs
// Rows: the tab is hidden (visibilityState), output far above the worker's window made meanwhile is on
// screen within 2 s of return, 200 tab switches, and data-terminal-reconnects/-replays never move.
// The terminal it opens is closed at the end. Terminal contents are never printed.
import { spawn } from "node:child_process"
import { readFileSync, rmSync } from "node:fs"

const { STATE, CHROME, CONSOLE_ORIGIN: origin, MACHINE: machine } = process.env
const hiddenMs = Number(process.env.HIDDEN_MS ?? 125000)
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
  const flooded = marker("hidden")
  await typeLine(page, `sleep 5; yes 0123456789 | head -c 4000000 | tr -d '\\n'; echo; echo ${flooded}`)
  const other = (await send("Target.createTarget", { url: "about:blank" })).targetId
  await sleep(1500)
  log((await evaluate(page, "document.visibilityState")) === "hidden", "the terminal tab is hidden", await evaluate(page, "document.visibilityState"))
  await sleep(hiddenMs)
  await send("Target.activateTarget", { targetId: first.targetId })
  const back = Date.now()
  log(await shows(page, flooded, 2000), "output made while hidden is on screen within 2 s of return", `${Date.now() - back} ms`)
  let after = await counts(page)
  log(after[0] === before[0] && after[1] === before[1], "hidden: reconnects and replays unchanged", `${before} -> ${after} close=${await attr(page, "data-terminal-close")}`)

  for (let i = 0; i < 200; i++) await send("Target.activateTarget", { targetId: i % 2 ? first.targetId : other })
  await send("Target.activateTarget", { targetId: first.targetId })
  await sleep(6000)
  after = await counts(page)
  log(after[0] === before[0] && after[1] === before[1], "200 tab switches: reconnects and replays unchanged", `${before} -> ${after}`)
  const echoed = marker("after")
  await typeLine(page, `echo ${echoed}`)
  log((await attr(page, "data-terminal-phase")) === "attached" && (await shows(page, echoed, 15000)), "still attached and echoes its marker")

  const closeButton = `[...document.querySelectorAll("button")].find((b) => (b.getAttribute("aria-label") || b.textContent).trim() === "Close terminal")`
  await evaluate(page, `${closeButton}?.click()`)
  await sleep(1000)
  await evaluate(page, `[...document.querySelectorAll("[role=alertdialog] button")].find((b) => (b.getAttribute("aria-label") || b.textContent).trim() === "Close terminal")?.click()`)
  log(await until(async () => /\/terminals$/.test(await evaluate(page, "location.pathname")), 20000), "the terminal is closed", terminal)
} finally {
  chrome.kill()
  rmSync(profile, { recursive: true, force: true })
}
console.log(failed ? `${failed} failed` : "all passed")
process.exit(failed ? 1 : 0)
