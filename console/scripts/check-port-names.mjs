// Browser check against the real server for Network > Port names and the machine page's Front desk: add a name per scope
// (the row shows the full address it opens, its scope and port), remove it, change the front desk and see it persist.
// Needs an account with one machine it manages (the check restores the front desk and removes its names at the end).
//   STATE=<storage state of that account> CHROME=<Chromium> CONSOLE_ORIGIN=<console origin> node check-port-names.mjs
import { chromium } from "playwright-core"

const { STATE, CHROME, CONSOLE_ORIGIN: origin } = process.env
for (const [name, value] of Object.entries({ STATE, CHROME, CONSOLE_ORIGIN: origin }))
  if (!value) throw new Error(`${name} is not set; see the header of check-port-names.mjs`)

let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const browser = await chromium.launch({ executablePath: CHROME })
const page = await (await browser.newContext({ storageState: STATE, viewport: { width: 1280, height: 900 } })).newPage()
const call = (method, path, body) =>
  page.evaluate(
    async ([method, path, body]) => {
      const res = await fetch(`/api/v1/${path}`, { method, headers: { "content-type": "application/json" }, body })
      return { status: res.status, data: (await res.json().catch(() => ({}))).data }
    },
    [method, path, body && JSON.stringify(body)],
  )
const names = ["c8pnmachine", "c8pnall", "c8pntop", "c8pnshadow"]
const row = (text) => page.locator("section#port-names div.min-h-14", { hasText: text })

await page.goto(`${origin}/network`, { waitUntil: "networkidle" })
const machine = (await call("GET", "machines?limit=1")).data.items[0]
if (!machine?.can_manage) throw new Error("the account needs one machine it manages")
const originalDesk = (await call("GET", `machines/${machine.id}/front-desk`)).data
const cleanup = async () => {
  const mine = (await call("GET", "workspaces")).data.items.find((w) => w.kind === "team" && w.role === "owner")
  if (mine)
    for (const item of (await call("GET", `port-names?workspace=${mine.id}`)).data?.items ?? [])
      if (names.includes(item.name)) await call("DELETE", `port-names/${item.id}?workspace=${mine.id}`)
  await call("PATCH", `machines/${machine.id}`, { alias: machine.alias })
  const items = (await call("GET", "port-names?workspace=personal")).data?.items ?? []
  for (const item of items.filter((i) => names.includes(i.name))) await call("DELETE", `port-names/${item.id}?workspace=personal`)
  await call("PUT", `machines/${machine.id}/front-desk`, originalDesk)
}
try {
  await cleanup()
  await page.reload({ waitUntil: "networkidle" })
  await page.locator("section#port-names").waitFor()

  const add = async (scopeLabel, name, port, pickMachine) => {
    await page.getByRole("button", { name: "Add name" }).first().click()
    const dialog = page.getByRole("dialog")
    await dialog.getByRole("button", { name: scopeLabel, exact: true }).click()
    if (pickMachine) {
      await dialog.getByText("Machine", { exact: true }).locator("xpath=following::button[1]").click()
      await page.getByRole("option").first().click()
    }
    await dialog.getByLabel("Name", { exact: true }).fill(name)
    await dialog.getByLabel("Port", { exact: true }).fill(String(port))
    await dialog.getByRole("button", { name: "Add name" }).click()
    await dialog.waitFor({ state: "hidden", timeout: 15000 }).catch(() => {})
  }
  await add("One machine", names[0], 8124, true)
  await add("All machines", names[1], 8123, false)
  await add("Top level", names[2], 8125, true)
  const listed = (await call("GET", "port-names?workspace=personal")).data.items
  log(names.slice(0, 3).every((n) => listed.some((i) => i.name === n)), "the three scopes were added", listed.map((i) => `${i.scope}`).join(","))

  const machineRow = row(`${names[0]}.${machine.alias}.`)
  log(await machineRow.getByText(/One machine · /).isVisible(), "machine name: full address, scope and machine", await machineRow.innerText().catch(() => "no row"))
  log(await machineRow.getByText("8124", { exact: true }).isVisible(), "machine name shows its port")
  log(await row(`${names[1]}.`).getByText("All machines", { exact: true }).isVisible(), "all-machines name shows its scope")
  const top = row(names[2]).first()
  const topAddress = (await top.innerText()).split("\n")[0]
  log(/^c8pntop\.[^.<]+\.[^.<]+/.test(topAddress) && !topAddress.includes("<machine>"), "top-level name is <name>.<domain>", topAddress)

  // Remove one through the row menu.
  await page.getByRole("button", { name: new RegExp(`^Actions for ${names[0]}\\.`) }).click()
  await page.getByRole("menuitem", { name: "Remove name" }).click()
  await page.getByRole("alertdialog").getByRole("button", { name: "Remove name" }).click()
  await machineRow.waitFor({ state: "detached", timeout: 15000 }).catch(() => {})
  const after = (await call("GET", "port-names?workspace=personal")).data.items
  log(!after.some((i) => i.name === names[0]) && after.some((i) => i.name === names[1]), "Remove name removes only that name")

  // A machine renamed to a name's label takes the address over: the row says so.
  await call("POST", "port-names?workspace=personal", { scope: "top_level", name: names[3], port: 8126, machine_id: machine.id })
  await call("PATCH", `machines/${machine.id}`, { alias: names[3] })
  await page.reload({ waitUntil: "networkidle" })
  log(await page.getByText(`A machine named ${names[3]} now uses this address.`).isVisible(), "a shadowed name says which machine uses its address")
  await call("PATCH", `machines/${machine.id}`, { alias: machine.alias })

  // Front desk on the machine page.
  await page.goto(`${origin}/machines/${machine.id}`, { waitUntil: "networkidle" })
  const desk = page.getByLabel("Front desk port")
  await desk.waitFor({ timeout: 20000 })
  log((await desk.inputValue()) === String(originalDesk.port), "front desk shows the saved port", await desk.inputValue())
  await page.getByRole("button", { name: originalDesk.scheme, exact: true }).click()
  await page.getByRole("menuitemradio", { name: originalDesk.scheme === "http" ? "https" : "http" }).click()
  await desk.fill("8443")
  await page.getByRole("button", { name: "Save", exact: true }).click()
  await page.getByText(/^Front desk set to /).waitFor({ timeout: 15000 }).catch(() => {})
  const saved = (await call("GET", `machines/${machine.id}/front-desk`)).data
  log(saved.port === 8443 && saved.scheme !== originalDesk.scheme, "Save stores scheme and port", JSON.stringify(saved))
  await page.reload({ waitUntil: "networkidle" })
  log((await page.getByLabel("Front desk port").inputValue()) === "8443", "front desk persists after reload")
  await desk.fill("70000")
  log(await page.getByLabel("Front desk port").getAttribute("aria-invalid") === "true" && await page.getByRole("button", { name: "Save", exact: true }).isDisabled(), "an invalid port cannot be saved")

  // Permissions: shown as a team member (role faked in the tab) and for a machine the viewer cannot manage.
  const team = (await call("GET", "workspaces")).data.items.find((w) => w.kind === "team" && w.role === "owner")
  if (team) {
    const wide = (await call("POST", `port-names?workspace=${team.id}`, { scope: "all_machines", name: names[1], port: 8127 })).data
    const member = await (await browser.newContext({ storageState: STATE, viewport: { width: 1280, height: 900 } })).newPage()
    await member.context().addCookies([{ name: "pb-workspace", value: team.id, url: origin }])
    await member.context().route("**/api/v1/workspaces", async (route) => {
      const real = await (await route.fetch()).json()
      real.data.items = real.data.items.map((w) => (w.id === team.id ? { ...w, role: "member" } : w))
      return route.fulfill({ contentType: "application/json", body: JSON.stringify(real) })
    })
    await member.goto(`${origin}/network`, { waitUntil: "networkidle" })
    await member.locator("section#port-names").getByText(`${names[1]}.`).first().waitFor({ timeout: 20000 })
    log((await member.getByRole("button", { name: /^Actions for c8pnall\./ }).count()) === 0, "a member sees an all-machines name without a remove menu")
    await member.getByRole("button", { name: "Add name" }).first().click()
    const dialog = member.getByRole("dialog")
    log(
      (await dialog.getByRole("button", { name: "All machines", exact: true }).isDisabled()) &&
        (await dialog.getByRole("button", { name: "Top level", exact: true }).isDisabled()) &&
        (await dialog.getByText(/need a workspace owner or admin/).isVisible()),
      "a member cannot pick All machines or Top level and is told why",
    )
    if (wide?.id) await call("DELETE", `port-names/${wide.id}?workspace=${team.id}`)
  } else log(false, "needs a team the account owns for the permission steps")

  const readonly = await (await browser.newContext({ storageState: STATE, viewport: { width: 1280, height: 900 } })).newPage()
  await readonly.context().route("**/api/v1/machines**", async (route) => {
    const response = await route.fetch()
    const body = await response.json()
    if (body.data?.items) body.data.items = body.data.items.map((m) => ({ ...m, can_manage: false }))
    else if (body.data?.id) body.data.can_manage = false
    return route.fulfill({ contentType: "application/json", body: JSON.stringify(body) })
  })
  await readonly.goto(`${origin}/machines/${machine.id}`, { waitUntil: "networkidle" })
  await readonly.getByText("Addresses that match no name go here.").waitFor({ timeout: 20000 })
  const readonlyDesk = readonly.locator("section", { hasText: "Front desk" }).last()
  await readonlyDesk.getByText(/ on \d+$/).waitFor({ timeout: 20000 }).catch(() => {})
  const text = (await readonlyDesk.innerText()).replace(/\n+/g, " | ")
  log((await readonly.getByLabel("Front desk port").count()) === 0 && / on \d+$/.test(text), "without machine management the front desk is read-only", text)
} finally {
  await cleanup()
  await browser.close()
}
process.exit(failed ? 1 : 0)
