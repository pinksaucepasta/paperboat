// The docked quick terminal keeps its height whatever it prints, and the wheel scrolls its scrollback,
// not the page: docked right and bottom, in WebKit (Safari) and Chromium.
// A signed-in storage state (tools/device-smoke/console_session.mjs) and an online machine:
//   STATE=state.json CONSOLE_ORIGIN=https://… MACHINE=machine_… [CMD="seq 500"] [BROWSERS=webkit,chromium] \
//   [PW_MODULE=playwright] node scripts/check-quick-terminal-scroll.mjs
// CMD must print a few hundred lines; for a Windows machine use CMD='dir C:\Windows\System32\*.exe'.
const playwright = await import(process.env.PW_MODULE ?? "playwright")
const { STATE, CONSOLE_ORIGIN, MACHINE, CMD = "seq 500", BROWSERS = "webkit,chromium" } = process.env
if (!STATE || !CONSOLE_ORIGIN || !MACHINE) throw new Error("set STATE, CONSOLE_ORIGIN and MACHINE")

const failures = []
for (const name of BROWSERS.split(",")) {
  const browser = await playwright[name].launch()
  for (const dock of ["right", "bottom"]) {
    const label = `${name} docked ${dock}`
    const context = await browser.newContext({ storageState: STATE, viewport: { width: 1470, height: 860 } })
    await context.addInitScript((d) => localStorage.setItem("pb-terminal-dock", d), dock)
    const page = await context.newPage()
    const measure = () =>
      page.evaluate(() => {
        const host = document.querySelector(".terminal-host")
        return {
          window: innerHeight,
          page: document.documentElement.scrollHeight,
          pageTop: Math.round(scrollY),
          height: host.clientHeight,
          content: host.scrollHeight,
          top: Math.round(host.scrollTop),
        }
      })
    const content = () => page.evaluate(() => document.querySelector(".terminal-host").scrollHeight)
    try {
      await page.goto(`${CONSOLE_ORIGIN}/machines/${MACHINE}?terminal=open`)
      await page.waitForSelector(".terminal-host .term-row", { timeout: 90_000 })
      await page.waitForFunction(() => document.querySelector(".terminal-host").textContent.trim().length > 0, null, { timeout: 90_000 })
      await page.waitForTimeout(3000)
      await page.locator(".terminal-host").click()
      // A shell that just started may drop keys (Windows), so retype until the listing arrives.
      let printed = false
      for (let attempt = 0; attempt < 4 && !printed; attempt++) {
        const start = await content()
        await page.keyboard.press("Enter")
        await page.keyboard.type(CMD, { delay: 20 })
        await page.keyboard.press("Enter")
        await page.waitForTimeout(10_000)
        printed = (await content()) > start + 1000
      }
      if (!printed) throw new Error("the command printed nothing")
      const before = await measure()
      const box = await page.locator(".terminal-host").boundingBox()
      await page.mouse.move(box.x + box.width / 2, Math.min(box.y + box.height / 2, before.window - 20))
      for (let i = 0; i < 5; i++) {
        await page.mouse.wheel(0, -300)
        await page.waitForTimeout(200)
      }
      await page.waitForTimeout(800)
      const after = await measure()
      console.log(label, JSON.stringify({ before, after }))
      if (after.page > after.window || after.pageTop !== 0) failures.push(`${label}: the page grew or scrolled`)
      if (before.content <= before.height + 300 || before.top <= 300) failures.push(`${label}: no scrollback to scroll`)
      if (after.top > before.top - 300) failures.push(`${label}: the wheel did not scroll the terminal`)
      if (process.env.SHOTS) await page.screenshot({ path: `${process.env.SHOTS}/${name}-${dock}.png` })
    } catch (error) {
      console.log(label, String(error))
      failures.push(`${label}: ${String(error).slice(0, 300)}`)
    }
    // Closing the panel ends its shell on the machine.
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
