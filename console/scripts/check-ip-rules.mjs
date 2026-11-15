// Browser check for W-10 / W-11: IP rules and the flood-protection notice on tunnel and preview pages.
//   STATE=<storage state of the owner> CHROME=<Chromium> CONSOLE_ORIGIN=<console origin> TUNNEL=<tunnel id>
//   [PREVIEW=<preview id>]  also checks the preview detail page
//   node check-ip-rules.mjs
// Saves rules through the chip inputs, reloads and sees them, shows the bad-entry error, restores the original rules,
// and compares the flood notice with GET /v1/tunnels/<id>/status. Nothing secret is printed.
import { chromium } from "playwright-core"

const { STATE, CHROME, CONSOLE_ORIGIN: origin, TUNNEL: tunnel, PREVIEW: preview } = process.env
for (const [name, value] of Object.entries({ STATE, CHROME, CONSOLE_ORIGIN: origin, TUNNEL: tunnel }))
  if (!value) throw new Error(`${name} is not set; see the header of check-ip-rules.mjs`)

const browser = await chromium.launch({ executablePath: CHROME })
const ctx = await browser.newContext({ storageState: STATE, viewport: { width: 1280, height: 900 } })
const page = await ctx.newPage()
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const api = async (path, init) => {
  const res = await ctx.request.fetch(`${origin}/api/v1/${path}`, init)
  return { status: res.status(), data: (await res.json().catch(() => null))?.data }
}
const chips = (name) =>
  page.locator(`#ip-rules-${name}`).locator("xpath=..").locator("[data-slot=badge]").allInnerTexts().then((all) => all.map((t) => t.trim()))
const add = async (name, value) => {
  await page.locator(`#ip-rules-${name}`).fill(value)
  await page.locator(`#ip-rules-${name}`).press("Enter")
}
const same = (a, b) => JSON.stringify(a) === JSON.stringify(b)

async function rules(kind, id) {
  const base = `${kind}s/${encodeURIComponent(id)}/ip-rules`
  const original = (await api(base)).data
  const shown = await page.locator("#ip-rules-allow").waitFor({ timeout: 30000 }).then(() => true, () => false)
  log(shown, `${kind}: IP rules section shows Allow and Deny`)
  if (!shown) return
  log(same(await chips("allow"), original.allow) && same(await chips("deny"), original.deny), `${kind}: chips match the server`)

  await add("deny", "not-an-address")
  await page.getByRole("button", { name: "Save", exact: true }).click()
  const error = await page.getByText(/not-an-address.*not an IP address/).waitFor({ timeout: 15000 }).then(() => true, () => false)
  log(error, `${kind}: invalid entry is refused naming it`)
  log(same((await api(base)).data, original), `${kind}: the refused save changed nothing`)

  await page.getByRole("button", { name: "Remove not-an-address" }).click()
  await add("allow", "203.0.113.0/24")
  await add("deny", "198.51.100.7")
  await page.getByRole("button", { name: "Save", exact: true }).click()
  await page.getByText("IP rules saved").waitFor({ timeout: 15000 }).catch(() => {})
  const stored = (await api(base)).data
  log(stored.allow.includes("203.0.113.0/24") && stored.deny.includes("198.51.100.7/32"), `${kind}: both lists saved in one request`)

  await page.reload({ waitUntil: "networkidle" })
  await page.locator("#ip-rules-allow").waitFor({ timeout: 30000 })
  log((await chips("allow")).includes("203.0.113.0/24") && (await chips("deny")).includes("198.51.100.7/32"), `${kind}: reload shows the saved chips`)

  const restored = await api(base, { method: "PUT", headers: { "content-type": "application/json", origin }, data: original })
  log(restored.status === 200 && same((await api(base)).data, original), `${kind}: original rules restored`)
}

try {
  await page.goto(`${origin}/tunnels/${encodeURIComponent(tunnel)}`, { waitUntil: "networkidle", timeout: 120000 })
  await page.getByText("Details", { exact: true }).waitFor({ timeout: 30000 })
  const flood = (await api(`tunnels/${encodeURIComponent(tunnel)}/status`)).data?.flood_protection
  await page.waitForTimeout(3000)
  const notice = page.locator("[data-flood-protection]")
  if (flood) {
    const text = await notice.innerText().catch(() => "")
    log(text.includes(String(flood.refused)) && text.includes(flood.summary), "tunnel: flood notice shows count and summary")
  } else log((await notice.count()) === 0, "tunnel: no flood notice without refusals")
  await rules("tunnel", tunnel)

  // No live tunnel carries refusals, so the notice is shown against the real status with flood_protection added.
  const since = "2026-11-16T04:00:00Z"
  const summary = "One address sent more requests than allowed."
  await page.route(`**/api/v1/tunnels/${encodeURIComponent(tunnel)}/status`, async (route) => {
    const real = await route.fetch()
    const body = await real.json()
    body.data = { ...body.data, flood_protection: { refused: 1234, since, summary } }
    await route.fulfill({ response: real, json: body })
  })
  await page.goto(`${origin}/tunnels/${encodeURIComponent(tunnel)}`, { waitUntil: "networkidle", timeout: 120000 })
  const injected = await page.locator("[data-flood-protection]").waitFor({ timeout: 30000 }).then(() => page.locator("[data-flood-protection]").innerText(), () => "")
  log(injected.includes("1234 requests refused since") && injected.includes(summary), "tunnel: flood notice shows count, since and summary (injected status)")
  await page.unroute(`**/api/v1/tunnels/${encodeURIComponent(tunnel)}/status`)

  if (preview) {
    await page.goto(`${origin}/previews/${encodeURIComponent(preview)}`, { waitUntil: "networkidle", timeout: 120000 })
    await page.getByText("Forwards to", { exact: true }).waitFor({ timeout: 30000 })
    log(true, "preview: detail page shows its details")
    await rules("preview", preview)
  }
} catch (error) {
  log(false, "check ran to the end", String(error).split("\n")[0])
} finally {
  await browser.close()
}
console.log(failed ? `${failed} failed` : "all passed")
process.exit(failed ? 1 : 0)
