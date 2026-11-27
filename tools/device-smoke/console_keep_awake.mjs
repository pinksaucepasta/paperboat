// F2-06, the console half: the machine page's Keep awake menu sets the policy the server holds and shows what the machine
// reports. On a machine that cannot keep itself awake the page must say so and offer no menu. Restores "Off".
// usage: ORIGIN=... STATE=... [PW_MODULE=...] node console_keep_awake.mjs <machine id>
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const origin = process.env.ORIGIN
const id = process.argv[2]
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
const context = await browser.newContext({ storageState: process.env.STATE, viewport: { width: 1280, height: 900 } })
const page = await context.newPage()
const machine = async () => (await (await context.request.get(`${origin}/api/v1/machines/${id}`)).json()).data
const until = async (probe, ms = 90000) => {
  for (const end = Date.now() + ms; Date.now() < end; await new Promise((r) => setTimeout(r, 1000))) {
    const found = await probe().catch(() => null)
    if (found) return found
  }
  return null
}
const cell = () => page.locator("dl > div", { has: page.getByText("Keep awake", { exact: true }) }).first()
try {
  await page.goto(`${origin}/machines/${id}`, { waitUntil: "domcontentloaded", timeout: 120000 })
  await cell().waitFor({ timeout: 60000 })
  const availability = (await until(async () => (await machine()).availability)) ?? null
  log(Boolean(availability), "the server reports the machine's availability", JSON.stringify(availability))
  if (availability?.status === "unsupported") {
    log(Boolean(await until(async () => /Not supported on this machine/.test(await cell().innerText()))), "the page says keep awake is not supported here")
    log((await cell().getByRole("button").count()) === 0, "and offers no menu")
  } else if (availability) {
    for (const [label, mode, shown] of [["On, stays awake", "keep_awake", "On"], ["Off, may sleep", "allow_sleep", "Off"]]) {
      await cell().getByRole("button").first().click({ timeout: 30000 })
      await page.getByRole("menuitemradio", { name: label }).click({ timeout: 15000 })
      log(Boolean(await until(async () => (await machine()).availability?.desired_mode === mode, 30000)), `choosing "${label}" is saved on the server`)
      const applied = await until(async () => {
        const now = (await machine()).availability
        return now?.status && now.status !== "pending" ? now : null
      })
      log(applied?.status === "applied" || applied?.status === "healthy" || applied?.status === "ok", `the machine reports ${mode} applied`, JSON.stringify(applied ?? (await machine()).availability))
      await page.reload({ waitUntil: "domcontentloaded" })
      log(Boolean(await until(async () => new RegExp(`Keep awake\\s*${shown}\\b`).test((await cell().innerText()).replace(/\n/g, " ")), 30000)), `the page shows ${shown} after a reload`, (await cell().innerText()).replace(/\n/g, " "))
    }
  }
} catch (error) {
  log(false, "keep awake check stopped", String(error.message).split("\n")[0].slice(0, 200))
} finally {
  await browser.close()
}
process.exit(failed ? 1 : 0)
