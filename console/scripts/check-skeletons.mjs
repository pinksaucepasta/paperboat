// Loading states must look like the page they stand in for. For each route: hold every browser API
// read, screenshot the loading state, release the reads, screenshot the loaded page, and measure:
//   - CLS from the moment the data lands (target 0: nothing that was on screen moves),
//   - controls present in both states (buttons, fields, tabs, headings, table headers, matched by
//     their text) must keep their box within 2 px,
//   - controls the loaded page shows above its first data row that the loading state lacked.
// usage: ORIGIN=<console origin> STATE=<playwright storage state> PW_CHROMIUM=<chromium> [PW_MODULE=<playwright-core
//        index.mjs>] [OUT=<screenshot dir>] [VIEWPORT=1440x900|390x844] [SCHEME=light|dark] [MACHINE=<machine id>]
//        node check-skeletons.mjs [route…]
// Read only: it only navigates. Exits 1 when a route shifts or a control moves.
import { mkdirSync } from "node:fs"

const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const origin = process.env.ORIGIN
const out = process.env.OUT
const [width, height] = (process.env.VIEWPORT ?? "1440x900").split("x").map(Number)
const scheme = process.env.SCHEME ?? "light"
const machine = process.env.MACHINE
const tabs = ["", "/terminals", "/previews", "/tunnels", "/files", "/environment", "/config", "/usage", "/activity"]
const routes = process.argv.slice(2).length
  ? process.argv.slice(2)
  : [
      "/", "/terminals", "/previews", "/tunnels", "/files", "/environment", "/config", "/domains", "/network",
      "/usage", "/activity", "/team", "/access", "/settings/api-tokens", "/billing", "/account",
      ...(machine ? tabs.map((tab) => `/machines/${machine}${tab}`) : []),
    ]
if (out) mkdirSync(out, { recursive: true })

const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
const context = await browser.newContext({
  viewport: { width, height },
  colorScheme: scheme,
  reducedMotion: "reduce",
  storageState: process.env.STATE,
})
await context.addInitScript(() => {
  window.__shifts = []
  new PerformanceObserver((list) => {
    for (const entry of list.getEntries()) {
      if (entry.hadRecentInput) continue
      const nodes = entry.sources.map((s) => s.node?.outerHTML?.slice(0, 90) ?? "?")
      window.__shifts.push({ t: entry.startTime, value: entry.value, nodes })
    }
  }).observe({ type: "layout-shift", buffered: true })
})

// Controls a person reads or clicks, keyed by role and text, with their boxes.
const controls = () =>
  [...document.querySelectorAll("main button, main input, main th, main h1, main h2, main [role=tab], main nav a")]
    .map((el) => {
      const r = el.getBoundingClientRect()
      const text = (el.getAttribute("aria-label") || el.getAttribute("placeholder") || el.textContent || "").trim()
      return { key: `${el.tagName.toLowerCase()}:${text.replace(/\s+/g, " ").replace(/\d+/g, "#").slice(0, 40)}`, x: r.x, y: r.y, w: r.width, h: r.height }
    })
    .filter((c) => c.w > 0 && c.h > 0 && c.y < innerHeight)

const name = (route) => (route === "/" ? "machines" : route.slice(1).replace(/[/?=&]/g, "_").replace(/machine_[0-9a-f-]+/, "machine"))
const rows = []
for (const route of routes) {
  const page = await context.newPage()
  let release
  const released = new Promise((resolve) => (release = resolve))
  await page.route("**/api/v1/**", async (r) => {
    await released
    await r.continue().catch(() => {})
  })
  await page.goto(origin + route, { waitUntil: "domcontentloaded" })
  await page.waitForTimeout(2500)
  const loading = await page.evaluate(controls)
  if (out) await page.screenshot({ path: `${out}/${name(route)}-${scheme}-${width}-loading.png` })
  const releasedAt = await page.evaluate(() => performance.now())
  release()
  await page.waitForLoadState("networkidle").catch(() => {})
  await page.waitForTimeout(2500)
  const loaded = await page.evaluate(controls)
  if (out) await page.screenshot({ path: `${out}/${name(route)}-${scheme}-${width}-loaded.png` })
  const shifts = (await page.evaluate(() => window.__shifts)).filter((s) => s.t >= releasedAt)
  const cls = shifts.reduce((sum, s) => sum + s.value, 0)
  // Only controls whose text is unique in both states can be matched (many rows share "Copy").
  const unique = (list) => new Map([...Map.groupBy(list, (c) => c.key)].filter(([, cs]) => cs.length === 1).map(([k, [c]]) => [k, c]))
  const byKey = new Map(loading.map((c) => [c.key, c]))
  const loadingOnce = unique(loading)
  const moved = [...unique(loaded).values()]
    .filter((c) => loadingOnce.has(c.key))
    .map((c) => ({ c, s: loadingOnce.get(c.key) }))
    .filter(({ c, s }) => Math.max(Math.abs(c.x - s.x), Math.abs(c.y - s.y), Math.abs(c.w - s.w), Math.abs(c.h - s.h)) > 2)
    .map(({ c, s }) => `${c.key} (${Math.round(s.x)},${Math.round(s.y)} ${Math.round(s.w)}x${Math.round(s.h)} → ${Math.round(c.x)},${Math.round(c.y)} ${Math.round(c.w)}x${Math.round(c.h)})`)
  // Toolbars and headings above the first table row or list that only appear once loaded.
  const firstRow = Math.min(...loaded.filter((c) => c.key.startsWith("th:")).map((c) => c.y + c.h), height)
  const missing = loaded.filter((c) => c.y < firstRow && !byKey.has(c.key) && !c.key.startsWith("th:")).map((c) => c.key)
  rows.push({ route, cls, moved, missing, shifts })
  await page.close()
}
await browser.close()

let failed = 0
for (const r of rows) {
  const ok = r.cls < 0.001 && r.moved.length === 0
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${r.route}  cls=${r.cls.toFixed(4)}${r.missing.length ? `  appears on load: ${r.missing.join(", ")}` : ""}`)
  for (const m of r.moved) console.log(`        moved: ${m}`)
  if (r.cls >= 0.001) for (const s of r.shifts.slice(0, 3)) console.log(`        shift ${s.value.toFixed(4)}: ${s.nodes[0]}`)
}
console.log(`${rows.length - failed}/${rows.length} passed (${width}x${height}, ${scheme})`)
process.exit(failed ? 1 : 0)
