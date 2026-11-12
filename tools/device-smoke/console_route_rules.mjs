// F8-16 console part: "Edit rules…" on a tunnel's hostname row opens the rules dialog, a header rule is saved and shown,
// the row says "1 rule", the dialog refuses an edge header with its reason, and the saved rule survives a reload.
// usage: ORIGIN=<console origin> STATE=<storage state, mode 600> PW_CHROMIUM=<chromium> [PW_MODULE=<playwright-core>] node console_route_rules.mjs <tunnel id>
// Prints one JSON line {check, status, steps}; exit 1 on failure. Nothing secret is printed.
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const tunnel = process.argv[2]
const steps = []
const step = async (name, fn) => {
  try {
    await fn()
    steps.push({ step: name, status: "PASS" })
  } catch (error) {
    steps.push({ step: name, status: "FAIL", detail: String(error).slice(0, 300) })
    throw error
  }
}
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
const context = await browser.newContext({ storageState: process.env.STATE, viewport: { width: 1280, height: 900 } })
const page = await context.newPage()
let failed = false
try {
  const open = async () => {
    await page.goto(`${process.env.ORIGIN}/tunnels/${tunnel}`)
    await page.getByRole("button", { name: /^Actions for / }).first().waitFor({ timeout: 60000 })
  }
  await step("the tunnel page lists its hostname with an actions menu", open)
  await step('the row menu has "Edit rules…"', async () => {
    await page.getByRole("button", { name: /^Actions for / }).first().click()
    await page.getByRole("menuitem", { name: "Edit rules…" }).waitFor({ timeout: 10000 })
  })
  await step("the dialog refuses an edge header with its reason", async () => {
    await page.getByRole("menuitem", { name: "Edit rules…" }).click()
    const dialog = page.getByRole("dialog")
    await dialog.getByText("Request headers").waitFor({ timeout: 10000 })
    await dialog.getByRole("button", { name: "Add" }).first().click()
    await dialog.getByLabel("Request headers name").fill("X-Forwarded-For")
    await dialog.getByText(/belongs to Paperboat/).waitFor({ timeout: 5000 })
    if (!(await dialog.getByRole("button", { name: "Save" }).isDisabled())) throw new Error("Save stayed enabled with an edge header")
  })
  await step("a header rule saves and the row says 1 rule", async () => {
    const dialog = page.getByRole("dialog")
    await dialog.getByLabel("Request headers name").fill("X-Console-Check")
    await dialog.getByLabel("Request headers value").fill("saved-from-console")
    await dialog.getByRole("button", { name: "Save" }).click()
    await page.getByText("1 rule", { exact: true }).waitFor({ timeout: 30000 })
  })
  await step("the rule survives a reload and shows in the dialog", async () => {
    await open()
    await page.getByText("1 rule", { exact: true }).waitFor({ timeout: 30000 })
    await page.getByRole("button", { name: /^Actions for / }).first().click()
    await page.getByRole("menuitem", { name: "Edit rules…" }).click()
    const value = await page.getByRole("dialog").getByLabel("Request headers value").inputValue()
    if (value !== "saved-from-console") throw new Error(`the saved value reads back as ${JSON.stringify(value)}`)
  })
} catch {
  failed = true
} finally {
  await browser.close()
}
console.log(JSON.stringify({ check: "console-route-rules", status: failed ? "FAIL" : "PASS", steps }))
process.exit(failed ? 1 : 0)
