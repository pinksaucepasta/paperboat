// Browser check for F17-06: restart the machine's runtime with the inspector page open; the page notices, reconnects,
// keeps its rows without duplicates and shows the next request; then the preview is stopped and the page says so.
// usage: ORIGIN=<console origin> STATE=<playwright storage state of the owner> RESTART_CMD='<restarts the runtime on the machine>'
//        STOP_CMD='<pb preview stop <id> on the machine>' node check-preview-inspector-restart.mjs <previewId> <previewHost>
import { chromium } from "playwright-core"
import { execSync } from "node:child_process"
const [id, host] = process.argv.slice(2)
const origin = process.env.ORIGIN
const exe = `${process.env.HOME}/.cache/ms-playwright/chromium_headless_shell-1248/chrome-headless-shell-linux-arm64/chrome-headless-shell`
const browser = await chromium.launch({ executablePath: exe })
const page = await (await browser.newContext({ storageState: process.env.STATE })).newPage()
const steps = []
const pass = (step, ok, detail = "") => { steps.push(ok); console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`) }
const until = async (fn, ms = 60000) => { for (let t = 0; t < ms; t += 500) { const v = await page.evaluate(fn).catch(() => null); if (v) return v; await page.waitForTimeout(500) } return null }
const phase = () => document.querySelector("[data-inspector-phase]")?.getAttribute("data-inspector-phase")
const ids = () => [...document.querySelectorAll("[data-capture-id]")].map((r) => r.getAttribute("data-capture-id"))
const hit = (p) => page.evaluate(([h, p]) => fetch(`https://${h}${p}`, { mode: "no-cors", cache: "no-store" }).catch(() => {}), [host, p])
let T0 = Date.now()
page.on("response", (r) => { if (r.url().includes("/api/v1/") && (r.status() >= 400 || r.url().includes("inspector"))) console.log(`net +${Math.round((Date.now() - T0) / 1000)}s ${r.request().method()} ${r.url().split("/api/v1/")[1].slice(0, 70)} ${r.status()}`) })
await page.goto(`${origin}/previews/${id}/inspect`)
pass("page connects", !!(await until(() => !!document.querySelector("[data-inspector-phase]"), 40000)))
await page.evaluate(() => {
  const s = [...document.querySelectorAll("label")].find((l) => l.textContent.trim() === "Capture")?.querySelector("[role=switch]")
  if (s?.getAttribute("aria-checked") !== "true") s.click()
})
await until(() => document.querySelector("[data-inspector-phase]")?.dataset.capture === "on")
const tag = `pbr-${Date.now()}`
await hit(`/${tag}/before`)
pass("request before restart is listed", !!(await until(() => [...document.querySelectorAll("[data-capture-id]")].some((r) => r.innerText.includes("/before")), 20000)))
const before = await page.evaluate(ids)
await page.evaluate(() => { window.__ph = []; setInterval(() => { const v = document.querySelector("[data-inspector-phase]")?.getAttribute("data-inspector-phase") ?? document.querySelector("[data-inspector-state]")?.getAttribute("data-inspector-state") ?? "none"; if (window.__ph.at(-1) !== v) window.__ph.push(v) }, 100) })
const t0 = Date.now(); T0 = t0
execSync(process.env.RESTART_CMD, { stdio: "inherit" })
pass("restart done", true, `${Date.now() - t0} ms`)
const lost = await until(() => window.__ph.includes("reconnecting"), 120000)
const tLost = Date.now() - t0
const back = await until(() => window.__ph.at(-1) === "ready" && window.__ph.includes("reconnecting"), 120000)
pass("page notices the loss, then reconnects by itself", !!lost && !!back, `lost after ${tLost} ms, ready again ${Date.now() - t0} ms after restart began; phases ${JSON.stringify(await page.evaluate(() => window.__ph))}`)
console.log("page text:", await page.evaluate(() => (document.querySelector("main main") ?? document.querySelector("main"))?.innerText.replace(/\s+/g, " ").slice(0, 300)))
let got = null
for (let i = 0; i < 20 && !got; i++) {
  await hit(`/${tag}/after${i}`)
  got = await until(() => [...document.querySelectorAll("[data-capture-id]")].some((r) => r.innerText.includes("/after")), 3000)
}
pass("a request after the restart arrives", !!got, `${Date.now() - t0} ms after restart began`)
const now = await page.evaluate(ids)
pass("rows kept and no duplicates", new Set(now).size === now.length && before.every((x) => now.includes(x)), `${before.length} -> ${now.length}`)
execSync(process.env.STOP_CMD.replaceAll("<id>", id), { stdio: "inherit" })
const stopped = await until(() => document.querySelector("[data-inspector-state]")?.getAttribute("data-inspector-state") === "Preview stopped" ? "Preview stopped" : null, 90000)
pass("after pb preview stop the page says so", stopped === "Preview stopped", `${stopped}; phases ${JSON.stringify(await page.evaluate(() => window.__ph))}`)
await browser.close()
process.exit(steps.every(Boolean) ? 0 : 1)
