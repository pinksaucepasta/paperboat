// View transitions (DESIGN.md, Motion and view transitions): between two sections, from the machines
// list into a machine and back, and from the Files machine list into a machine's Files, the page
// cross-fades or the machine's name moves; with reduced motion no transition starts at all; typing in
// the machines search never starts one; no page error or console error on the way.
//   STATE=<playwright storage state> CHROME=<Chromium> CONSOLE_ORIGIN=<console origin> [PW_MODULE=<playwright-core index.mjs>]
//   [RUNS=desktop,desktop-reduced,phone,phone-reduced] [DEBUG=1: print each navigation's transitions]
//   [SHOTS=<dir>: also records a video and a slowed frame sequence of each navigation, desktop and phone] node check-view-transitions.mjs
// Uses the first machine of the signed-in account's list; nothing is changed.
import { mkdirSync } from "node:fs"
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const { STATE, CHROME, CONSOLE_ORIGIN: origin, SHOTS: shots } = process.env
for (const [name, value] of Object.entries({ STATE, CHROME, CONSOLE_ORIGIN: origin }))
  if (!value) throw new Error(`${name} is not set; see the header of check-view-transitions.mjs`)
if (shots) mkdirSync(shots, { recursive: true })
const browser = await chromium.launch({ executablePath: CHROME })
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}

// Every document.startViewTransition call, with the pseudo-element animations that ran once it was ready.
const recorder = () => {
  window.__vt = []
  const start = document.startViewTransition.bind(document)
  document.startViewTransition = (...args) => {
    const transition = start(...args)
    const entry = { pseudos: [], groups: 0 }
    window.__vt.push(entry)
    transition.ready.then(
      () => {
        for (const animation of document.getAnimations()) {
          const pseudo = animation.effect?.pseudoElement ?? ""
          if (!pseudo.startsWith("::view-transition")) continue
          entry.pseudos.push(pseudo)
          if (pseudo.startsWith("::view-transition-group")) entry.groups++
        }
      },
      () => {},
    )
    return transition
  }
}

async function run(name, { reducedMotion, viewport, video }) {
  const ctx = await browser.newContext({
    storageState: STATE,
    viewport,
    reducedMotion: reducedMotion ? "reduce" : "no-preference",
    ...(video && { recordVideo: { dir: video, size: viewport } }),
  })
  await ctx.addInitScript(recorder)
  const page = await ctx.newPage()
  const errors = []
  page.on("pageerror", (error) => errors.push(error.message))
  page.on(
    "console",
    (message) => message.type() === "error" && errors.push(`${message.text()} ${message.location().url}`),
  )
  const phone = viewport.width < 600
  const transitions = () => page.evaluate(() => window.__vt.length)
  // Runs `act`, waits for the route, and returns the transitions it started once they have played.
  const step = async (label, act, until) => {
    const before = await transitions()
    // With SHOTS the document's animations run at a tenth of their speed and are photographed mid-way.
    const slow = shots && !reducedMotion && (await ctx.newCDPSession(page))
    if (slow) {
      await slow.send("Animation.enable")
      await slow.send("Animation.setPlaybackRate", { playbackRate: 0.1 })
    }
    await act()
    for (let frame = 0; slow && frame < 5; frame++) {
      await page.screenshot({ path: `${shots}/${name}-${label}-frame${frame}.png` })
      await page.waitForTimeout(250)
    }
    if (slow) await slow.send("Animation.setPlaybackRate", { playbackRate: 1 })
    await page.waitForURL(until, { timeout: 30000 })
    await page.waitForTimeout(1200)
    const all = await page.evaluate(() => window.__vt)
    const started = all.slice(before)
    if (process.env.DEBUG) console.log(label, JSON.stringify(started))
    if (shots) await page.screenshot({ path: `${shots}/${name}-${label}.png` })
    return started
  }
  const animated = (started) => started.some((entry) => entry.pseudos.length)
  const morphed = (started) => started.some((entry) => entry.groups)

  await page.goto(origin + "/", { waitUntil: "networkidle", timeout: 120000 })
  const row = page.locator("tbody a[href^='/machines/']").first()
  await row.waitFor({ timeout: 30000 })
  const machine = (await row.getAttribute("href")).split("/").pop()
  await page.waitForTimeout(1500)

  if (phone)
    await page
      .getByRole("button", { name: /toggle sidebar/i })
      .first()
      .click()
  const sections = await step(
    "1-section",
    () => page.locator("[data-sidebar='menu-button'][href='/files']").click(),
    /\/files$/,
  )
  log(
    reducedMotion ? !animated(sections) : animated(sections),
    `${name}: Machines to Files ${reducedMotion ? "starts no transition" : "cross-fades"}`,
    `${sections.length} started`,
  )
  const chooser = page.locator("tbody a[data-machine]").first()
  await chooser.waitFor({ timeout: 30000 })
  const picked = (await chooser.getAttribute("href")).split("=").pop()
  const files = await step("2-files-machine", () => chooser.click(), /\/files\?machine=/)
  log(
    reducedMotion ? !animated(files) : morphed(files),
    `${name}: Files list to a machine's Files ${reducedMotion ? "starts no transition" : "moves the machine's name"}`,
    `${picked.slice(0, 14)}`,
  )

  await page.goto(origin + "/", { waitUntil: "networkidle", timeout: 120000 })
  await row.waitFor({ timeout: 30000 })
  const detail = await step("3-machine", () => row.click(), new RegExp(`/machines/${machine}$`))
  log(
    reducedMotion ? !animated(detail) : morphed(detail),
    `${name}: machine row to the machine page ${reducedMotion ? "starts no transition" : "moves the machine's name"}`,
  )
  const back = await step(
    "4-back",
    () => page.locator("nav[aria-label='breadcrumb'] a[href='/']").click(),
    (url) => url.pathname === "/",
  )
  log(
    reducedMotion ? !animated(back) : morphed(back),
    `${name}: the breadcrumb back to the list ${reducedMotion ? "starts no transition" : "moves the name back"}`,
  )

  const typed = await transitions()
  await page
    .getByPlaceholder(/search/i)
    .first()
    .pressSequentially("zz", { delay: 120 })
  await page.waitForTimeout(1500)
  log((await transitions()) === typed, `${name}: typing in the search starts no transition`)
  log(errors.length === 0, `${name}: no page or console errors`, errors.slice(0, 2).join(" | ").slice(0, 200))
  await ctx.close()
}

const desktop = { width: 1280, height: 800 }
const phone = { width: 390, height: 844 }
const runs = {
  desktop: () => run("desktop", { viewport: desktop, video: shots && `${shots}/video-desktop` }),
  "desktop-reduced": () => run("desktop-reduced", { viewport: desktop, reducedMotion: true }),
  phone: () => run("phone", { viewport: phone, video: shots && `${shots}/video-phone` }),
  "phone-reduced": () => run("phone-reduced", { viewport: phone, reducedMotion: true }),
}
for (const name of (process.env.RUNS ?? Object.keys(runs).join(",")).split(",")) await runs[name]()
await browser.close()
console.log(failed ? `${failed} failed` : "all passed")
process.exit(failed ? 1 : 0)
