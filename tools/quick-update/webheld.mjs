// webheld.mjs: a held browser terminal for the update gate. Joins one terminal session as a guest with a join
// code through the console's own page (/terminal/<id>, the WASM browser terminal), waits for
// data-terminal-phase=attached, types an echo ticker (a marker every 200 ms) and records when each new marker
// first shows on screen. Terminal contents are never written: only marker indexes, times and phases.
// Env: PB_CONSOLE_ORIGIN, PW_CHROMIUM, PW_MODULE, SESSION, CODEFILE, OUT (events jsonl), STOP (file), MAX_S.
import { readFileSync, existsSync, appendFileSync } from "node:fs"
const { chromium } = await import(process.env.PW_MODULE)
const { PB_CONSOLE_ORIGIN: origin, PW_CHROMIUM: exe, SESSION: id, CODEFILE, OUT, STOP } = process.env
const maxS = Number(process.env.MAX_S ?? "900")
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const ev = (o) => appendFileSync(OUT, JSON.stringify({ t: Date.now() / 1000, ms: null, ...o }) + "\n")

const browser = await chromium.launch({ executablePath: exe })
const page = await (await browser.newContext()).newPage()
const phase = () => page.evaluate(() => document.querySelector("[data-terminal-phase]")?.getAttribute("data-terminal-phase") ?? "none")
const screen = () => page.evaluate(() => document.querySelector(".terminal-host")?.innerText ?? "")
try {
  await page.goto(`${origin}/terminal/${encodeURIComponent(id)}`, { timeout: 60_000 })
  await page.getByRole("button", { name: "Enter it" }).click({ timeout: 30_000 })
  await page.locator("#join-code").fill(readFileSync(CODEFILE, "utf8").trim())
  await page.locator("#join-name").fill("gap-verdict")
  await page.getByRole("button", { name: "Join" }).click()
  const t0 = Date.now()
  while ((await phase().catch(() => "none")) !== "attached") {
    if (Date.now() - t0 > 90_000) throw new Error(`not attached: ${await phase().catch(() => "none")}`)
    await sleep(250)
  }
  ev({ f: "term_web_attached", ok: true, ms: Date.now() - t0 })
  await page.locator(".terminal-host").click()
  // The typed line never contains PBWEB_<digits>; only the shell's output does.
  await page.keyboard.type("i=0; while [ $i -lt 100000 ]; do printf '%s_%s\\n' PBWEB $i; i=$((i+1)); sleep 0.2; done", { delay: 2 })
  await page.keyboard.press("Enter")
  let last = -1, lastAt = Date.now(), lastPhase = "attached", stalledAt = 0
  const end = Date.now() + maxS * 1000
  while (!existsSync(STOP) && Date.now() < end) {
    const text = await screen().catch(() => "")
    let max = -1
    for (const m of text.matchAll(/PBWEB_(\d+)/g)) max = Math.max(max, Number(m[1]))
    if (max > last) { ev({ f: "term_web", ok: true, i: max }); last = max; lastAt = Date.now(); stalledAt = 0 }
    const p = await phase().catch(() => "none")
    if (p !== lastPhase) { ev({ f: "term_web_phase", ok: p === "attached", phase: p }); lastPhase = p }
    // A stream that shows nothing new for 30 s has failed; one failure per 30 s while it lasts.
    if (last >= 0 && Date.now() - lastAt > 30_000 && Date.now() - stalledAt > 30_000) { ev({ f: "term_web", ok: false, why: "no new marker 30 s" }); stalledAt = Date.now() }
    await sleep(100)
  }
  await page.keyboard.press("Control+C").catch(() => {})
  ev({ f: "term_web_end", ok: true, last })
} catch (e) {
  ev({ f: "term_web", ok: false, why: String(e).slice(0, 160) })
  process.exitCode = 1
} finally {
  await browser.close()
}
