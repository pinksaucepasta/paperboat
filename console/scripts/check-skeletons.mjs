// Loading states must look like the page they stand in for. For each route it captures three states
// at the same viewport and compares them with the loaded page:
//   - shell: the HTML with JavaScript off, i.e. the static shell that paints before the streamed,
//     server-filled page arrives (what a slow load shows first);
//   - loading: JavaScript on, every browser /api/v1 read held;
//   - loaded: the reads released.
// It fails a route when, in the shell or the loading state:
//   - something on the loaded page (a text, an icon, a field, above the fold) has no element or
//     placeholder at its position (coverage below 90%), or a placeholder sits where nothing loads;
//   - a text is a literal "…" stand-in or a date in another format than the loaded one;
//   - after the reads are released, anything shifts (CLS > 0) or a button, field, tab, heading or
//     column header moves by more than 2 px.
// usage: ORIGIN=<console origin> STATE=<playwright storage state> PW_CHROMIUM=<chromium> [PW_MODULE=<playwright-core
//        index.mjs>] [OUT=<screenshot dir>] [VIEWPORT=1440x900|390x844] [SCHEME=light|dark] [MACHINE=<machine id>]
//        [WARM=1: visit each route once first, as a returning visitor whose lists remember their row counts]
//        node check-skeletons.mjs [route…]
// Read only: it only navigates. Exits 1 when a route fails.
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
const options = { viewport: { width, height }, colorScheme: scheme, reducedMotion: "reduce", storageState: process.env.STATE }
const context = await browser.newContext(options)
const shellContext = await browser.newContext({ ...options, javaScriptEnabled: false })
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

// What a person sees in the page body, above the fold: texts, icons, fields and placeholders.
const leaves = () =>
  [...document.querySelectorAll("main *")]
    .filter((el) => {
      if (el.closest("[hidden]") || (el.closest("svg") && el.tagName.toLowerCase() !== "svg")) return false
      const r = el.getBoundingClientRect()
      // Above the fold and on screen (text overflowing past the right edge can't be placed).
      if (r.width < 2 || r.height < 2 || r.bottom <= 48 || r.top >= innerHeight || r.left >= innerWidth) return false
      const style = getComputedStyle(el)
      if (style.visibility === "hidden" || style.opacity === "0") return false
      const text = [...el.childNodes].some((node) => node.nodeType === 3 && node.textContent.trim())
      return text || el.dataset.slot === "skeleton" || ["svg", "img", "input", "canvas"].includes(el.tagName.toLowerCase())
    })
    .map((el) => {
      const r = el.getBoundingClientRect()
      const text = [...el.childNodes].filter((node) => node.nodeType === 3).map((node) => node.textContent).join("").trim()
      // A control's own small icon (a row's "…") comes with its control; it needs no placeholder.
      const controlIcon = el.tagName.toLowerCase() === "svg" && r.width <= 16 && Boolean(el.closest("button"))
      return { x: r.x, y: r.y, w: r.width, h: r.height, text: text.slice(0, 60), skeleton: el.dataset.slot === "skeleton", controlIcon }
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

const overlap = (a, b) => {
  const w = Math.min(a.x + a.w, b.x + b.w) - Math.max(a.x, b.x)
  const h = Math.min(a.y + a.h, b.y + b.h) - Math.max(a.y, b.y)
  return w > 0 && h > 0 ? (w * h) / Math.min(a.w * a.h, b.w * b.h) : 0
}
const date = /\b\d{4}-\d{2}-\d{2}\b|\bUTC\b|\b(Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)[a-z]* \d{1,2}\b|\b\d{1,2} (Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)\b/

/** How an early state (shell or loading) differs from the loaded page, element by element. */
function compare(early, loaded) {
  const counted = loaded.filter((l) => !l.controlIcon)
  const missing = counted.filter((l) => !early.some((e) => overlap(e, l) > 0.3))
  // Status-dot placeholders (under 10 px) sit after a name of unknown width; they can't line up.
  const stray = early.filter((e) => e.skeleton && e.w >= 10 && !loaded.some((l) => overlap(e, l) > 0.3))
  const coverage = counted.length ? 1 - missing.length / counted.length : 1
  const texts = []
  for (const e of early.filter((e) => e.text)) {
    const there = loaded.find((l) => l.text && overlap(e, l) > 0.5)
    if (/…|\.\.\./.test(e.text) && there && there.text !== e.text) texts.push(`"${e.text}" stands in for "${there.text}"`)
    if (date.test(e.text) && there && date.test(there.text) && there.text !== e.text) texts.push(`date "${e.text}" becomes "${there.text}"`)
  }
  return { coverage, missing, stray, texts }
}

const name = (route) => (route === "/" ? "machines" : route.slice(1).replace(/[/?=&]/g, "_").replace(/machine_[0-9a-f-]+/, "machine"))
const shot = (page, route, state) => out && page.screenshot({ path: `${out}/${name(route)}-${scheme}-${width}-${state}.png` })
const rows = []
for (const route of routes) {
  const shellPage = await shellContext.newPage()
  await shellPage.goto(origin + route, { waitUntil: "load" })
  // The shell is hidden for its first 300 ms (globals.css .shell-delay); judge it once visible.
  await shellPage.waitForTimeout(500)
  const shell = await shellPage.evaluate(leaves)
  await shot(shellPage, route, "shell")
  await shellPage.close()

  if (process.env.WARM) {
    const warm = await context.newPage()
    await warm.goto(origin + route)
    await warm.waitForLoadState("networkidle").catch(() => {})
    await warm.waitForTimeout(1500)
    await warm.close()
  }
  const page = await context.newPage()
  let release
  const released = new Promise((resolve) => (release = resolve))
  await page.route("**/api/v1/**", async (r) => {
    await released
    await r.continue().catch(() => {})
  })
  await page.goto(origin + route, { waitUntil: "domcontentloaded" })
  await page.waitForTimeout(2500)
  const loadingControls = await page.evaluate(controls)
  const loading = await page.evaluate(leaves)
  await shot(page, route, "loading")
  const releasedAt = await page.evaluate(() => performance.now())
  release()
  await page.waitForLoadState("networkidle").catch(() => {})
  await page.waitForTimeout(2500)
  const loadedControls = await page.evaluate(controls)
  const loaded = await page.evaluate(leaves)
  await shot(page, route, "loaded")
  const shifts = (await page.evaluate(() => window.__shifts)).filter((s) => s.t >= releasedAt)
  const cls = shifts.reduce((sum, s) => sum + s.value, 0)
  // Only controls whose text is unique in both states can be matched (many rows share "Copy").
  const unique = (list) => new Map([...Map.groupBy(list, (c) => c.key)].filter(([, cs]) => cs.length === 1).map(([k, [c]]) => [k, c]))
  const loadingOnce = unique(loadingControls)
  const moved = [...unique(loadedControls).values()]
    .filter((c) => loadingOnce.has(c.key))
    .map((c) => ({ c, s: loadingOnce.get(c.key) }))
    .filter(({ c, s }) => Math.max(Math.abs(c.x - s.x), Math.abs(c.y - s.y), Math.abs(c.w - s.w), Math.abs(c.h - s.h)) > 2)
    .map(({ c, s }) => `${c.key} (${Math.round(s.x)},${Math.round(s.y)} ${Math.round(s.w)}x${Math.round(s.h)} → ${Math.round(c.x)},${Math.round(c.y)} ${Math.round(c.w)}x${Math.round(c.h)})`)
  rows.push({ route, cls, moved, shifts, shell: compare(shell, loaded), loading: compare(loading, loaded) })
  await page.close()
}
await browser.close()

let failed = 0
const describe = (label, c) => {
  const lines = []
  if (c.coverage < 0.9) {
    const sample = c.missing.slice(0, 4).map((m) => (m.text ? `"${m.text}"` : `${Math.round(m.w)}x${Math.round(m.h)}`) + `@${Math.round(m.x)},${Math.round(m.y)}`)
    lines.push(`${label}: ${Math.round(c.coverage * 100)}% of the loaded page has something in place; missing ${sample.join(", ")}`)
  }
  if (c.stray.length > 2) lines.push(`${label}: ${c.stray.length} placeholders where nothing loads`)
  for (const t of c.texts.slice(0, 3)) lines.push(`${label}: ${t}`)
  return lines
}
for (const r of rows) {
  const problems = [...describe("shell", r.shell), ...describe("loading", r.loading)]
  const ok = r.cls < 0.001 && r.moved.length === 0 && problems.length === 0
  if (!ok) failed++
  console.log(
    `${ok ? "PASS" : "FAIL"}  ${r.route}  cls=${r.cls.toFixed(4)}  shell=${Math.round(r.shell.coverage * 100)}%  loading=${Math.round(r.loading.coverage * 100)}%`,
  )
  for (const p of problems) console.log(`        ${p}`)
  for (const m of r.moved) console.log(`        moved: ${m}`)
  if (r.cls >= 0.001) for (const s of r.shifts.slice(0, 3)) console.log(`        shift ${s.value.toFixed(4)}: ${s.nodes[0]}`)
}
console.log(`${rows.length - failed}/${rows.length} passed (${width}x${height}, ${scheme})`)
process.exit(failed ? 1 : 0)
