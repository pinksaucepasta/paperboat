// Keys typed into the quick terminal before it attaches arrive: each line is typed exactly once, on the
// first connect and during a reconnect attempt (the old prompt is still up then), and the shell must print it.
// A signed-in storage state (tools/device-smoke/console_session.mjs) and an online machine:
//   STATE=state.json CONSOLE_ORIGIN=https://… MACHINE=machine_… [BROWSERS=webkit,chromium] [TRIALS=5] \
//   [TICKET_DELAY_MS=4000] [PW_MODULE=playwright] node scripts/check-quick-terminal-early-input.mjs
// TICKET_DELAY_MS holds the terminal ticket back so the typing lands before the attach (a line typed in any
// other phase then fails: it proves nothing); 0 types as early as an unslowed page allows.
const playwright = await import(process.env.PW_MODULE ?? "playwright")
const { STATE, CONSOLE_ORIGIN, MACHINE, BROWSERS = "webkit,chromium" } = process.env
const trials = Number(process.env.TRIALS ?? 5)
const ticketDelay = Number(process.env.TICKET_DELAY_MS ?? 4000)
if (!STATE || !CONSOLE_ORIGIN || !MACHINE) throw new Error("set STATE, CONSOLE_ORIGIN and MACHINE")

const failures = []
for (const name of BROWSERS.split(",")) {
  const browser = await playwright[name].launch()
  for (let trial = 1; trial <= trials; trial++) {
    const context = await browser.newContext({ storageState: STATE, viewport: { width: 1470, height: 860 } })
    const page = await context.newPage()
    await page.route("**/browser-ticket", async (route) => {
      await new Promise((done) => setTimeout(done, ticketDelay))
      await route.continue().catch(() => {})
    })
    const phase = (value, timeout = 90_000) => page.waitForSelector(`[data-terminal-phase="${value}"]`, { timeout })
    const state = () => page.evaluate(() => document.querySelector("[data-terminal-phase]")?.dataset.terminalPhase)
    // The shell printed the token on a row of its own: the line arrived whole and ran.
    const printed = (token) =>
      page.waitForFunction(
        (want) => [...document.querySelectorAll(".terminal-host .term-row")].some((row) => row.textContent.trim() === want),
        token,
        { timeout: 60_000 },
      )
    const typeOnce = async (when) => {
      const token = `hi${trial}${when}${Date.now() % 100000}abcdefghij`
      const label = `${name} trial ${trial} ${when}`
      const typedIn = await state()
      await page.locator(".terminal-host").click()
      await page.keyboard.type(`echo ${token}`, { delay: 20 })
      await page.keyboard.press("Enter")
      try {
        await printed(token)
        // Exactly once: a second send would print it again.
        await page.waitForTimeout(2000)
        const times = await page.evaluate(
          (want) => [...document.querySelectorAll(".terminal-host .term-row")].filter((row) => row.textContent.trim() === want).length,
          token,
        )
        const held = ticketDelay === 0 || typedIn === "connecting" || typedIn === "reconnecting"
        if (times === 1 && held) console.log(label, `PASS typed from phase ${typedIn}`)
        else {
          const why = times !== 1 ? `printed ${times} times` : `typed in phase ${typedIn}, not before the attach`
          console.log(label, `FAIL ${why}`)
          failures.push(`${label}: ${why}`)
        }
      } catch {
        const rows = await page.evaluate(() =>
          [...document.querySelectorAll(".terminal-host .term-row")].map((row) => row.textContent.trimEnd()).filter(Boolean).slice(-6),
        )
        console.log(label, `FAIL typed from phase ${typedIn}`, JSON.stringify(rows))
        failures.push(`${label}: typed from phase ${typedIn}, the shell never printed it`)
      }
    }
    try {
      await page.goto(`${CONSOLE_ORIGIN}/machines/${MACHINE}?terminal=open`)
      await page.waitForSelector(".terminal-host textarea", { state: "attached", timeout: 90_000 })
      await typeOnce("first")
      await phase("attached")
      await context.setOffline(true)
      await phase("reconnecting", 15_000)
      await context.setOffline(false)
      await phase("connecting", 15_000)
      await typeOnce("reconnect")
    } catch (error) {
      console.log(name, trial, String(error))
      failures.push(`${name} trial ${trial}: ${String(error).slice(0, 300)}`)
    }
    // Closing the panel ends its shell on the machine.
    await context.setOffline(false).catch(() => {})
    await page.getByRole("button", { name: "Close terminal" }).click({ timeout: 3000 }).catch(() => {})
    await new Promise((done) => setTimeout(done, 1000))
    await context.close().catch(() => {})
  }
  await browser.close().catch(() => {})
}
if (failures.length) {
  console.error("FAIL\n" + failures.join("\n"))
  process.exit(1)
}
console.log("PASS")
