// Network > Your regions on the live console: a throwaway account creates a region it names, sees it as its own (page and
// server), is refused the same name, renames it and removes it.
// usage: ORIGIN=... STATE=<a throwaway account> [PW_MODULE=...] node console_regions.mjs
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const origin = process.env.ORIGIN
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
const context = await browser.newContext({ storageState: process.env.STATE, viewport: { width: 1280, height: 900 } })
const page = await context.newPage()
const regions = async () => {
  const reply = await context.request.get(`${origin}/api/v1/selfhost/regions?scope_kind=account`)
  if (!reply.ok()) throw new Error(`GET selfhost/regions answered ${reply.status()}`)
  const data = (await reply.json()).data
  return data.items ?? data.regions ?? data
}
const until = async (probe, ms = 30000) => {
  for (const end = Date.now() + ms; Date.now() < end; await new Promise((r) => setTimeout(r, 500))) {
    const found = await probe().catch(() => false)
    if (found) return found
  }
  return false
}
const shown = (name) => page.getByText(name, { exact: true }).first()
const create = async (name) => {
  await page.getByRole("button", { name: "New region" }).first().click({ timeout: 30000 })
  const dialog = page.getByRole("dialog")
  await dialog.locator("input").fill(name)
  await dialog.getByRole("button", { name: "Create region" }).click()
  return dialog
}
try {
  await page.goto(`${origin}/network`, { waitUntil: "domcontentloaded", timeout: 120000 })
  log(Boolean(await until(async () => (await page.getByRole("heading", { name: "Your regions" }).count()) > 0, 60000)), "Network has a Your regions section")
  await create("Row lab")
  log(Boolean(await until(async () => (await regions()).some((r) => r.name === "Row lab"))), "the region is created on the server")
  log(Boolean(await until(async () => (await shown("Row lab").isVisible()))), "its card shows the chosen name")
  const again = await create("Row lab")
  log(Boolean(await until(async () => /already/i.test(await again.innerText()), 20000)), "the same name is refused with a sentence", (await again.innerText().catch(() => "")).replace(/\s+/g, " ").slice(0, 160))
  await again.getByRole("button", { name: "Cancel" }).click()
  await page.getByRole("button", { name: "Actions for Row lab" }).click()
  await page.getByRole("menuitem", { name: "Rename" }).click()
  const rename = page.getByRole("dialog")
  await rename.locator("input").fill("Row lab two")
  await rename.getByRole("button", { name: "Rename" }).click()
  log(Boolean(await until(async () => (await regions()).some((r) => r.name === "Row lab two") && !(await regions()).some((r) => r.name === "Row lab"))), "rename is saved on the server")
  await page.reload({ waitUntil: "domcontentloaded" })
  log(Boolean(await until(async () => shown("Row lab two").isVisible(), 60000)), "the new name is on the page after a reload")
  await page.getByRole("button", { name: "Actions for Row lab two" }).click()
  await page.getByRole("menuitem", { name: "Remove region" }).click()
  const remove = page.locator("[role=alertdialog], [role=dialog]")
  await remove.locator("input").fill("Row lab two") // removal asks for the region's name
  await remove.getByRole("button", { name: "Remove region" }).click()
  log(Boolean(await until(async () => !(await regions()).some((r) => r.name === "Row lab two"))), "Remove region removes it on the server")
  log(Boolean(await until(async () => (await page.getByRole("heading", { name: /Run your own servers/ }).count()) > 0 || (await page.getByText("Run your own servers in a region you name").count()) > 0)), "the empty state comes back")
} catch (error) {
  log(false, "regions check stopped", String(error.message).split("\n")[0].slice(0, 200))
} finally {
  await browser.close()
}
process.exit(failed ? 1 : 0)
