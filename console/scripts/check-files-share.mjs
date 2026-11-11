// Share a file from the Files page. Signed-in console, live machine:
//   STATE=<playwright storage state> CHROME=<Chromium> CONSOLE_ORIGIN=<console origin> [PW_MODULE=<playwright-core index.mjs>]
//   [MACHINE=<alias>] [SHOTS=<dir for screenshots>] node check-files-share.mjs
// Layout (needs no hosting): a machine's Files page has "Share a file" next to "Send files"; the Choose machine page
// has both too; Previews has no "Share a file" and no page-wide drop; a file's row menu has "Share link…"; the dialog
// opens with one defaults line (Hosted on <machine> · Anyone with the link · 24 hours) and its drop zone.
// Hosting (needs the server and machine to serve shared files): sharing a new HTML file from the dialog gives a live link
// whose page shows the file's content; "Share link…" on an Inbox file does too. Prints PASS/FAIL per step; exits 1 on a FAIL.
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const { STATE, CHROME, CONSOLE_ORIGIN: origin, MACHINE: alias = "dadape", SHOTS: shots } = process.env
for (const [name, value] of Object.entries({ STATE, CHROME, CONSOLE_ORIGIN: origin }))
  if (!value) throw new Error(`${name} is not set; see the header of check-files-share.mjs`)
const browser = await chromium.launch({ executablePath: CHROME })
const ctx = await browser.newContext({ storageState: STATE, viewport: { width: 1280, height: 900 } })
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const page = await ctx.newPage()
await page.goto(`${origin}/files`, { waitUntil: "networkidle", timeout: 120000 })
const listing = await ctx.request.get(`${origin}/api/v1/machines?workspace=personal&limit=200`)
const machine = ((await listing.json()).data ?? {}).items?.find((m) => m.alias === alias && m.online)
if (!machine) throw new Error(`${alias} is not online`)
const shot = (name) => (shots ? page.screenshot({ path: `${shots}/${name}.png` }) : undefined)
const dialog = page.getByRole("dialog")
const stamp = Date.now()

// Layout
await shot("share-choose-machine")
const toolbar = (p) => p.getByRole("button", { name: "Share a file" })
log((await toolbar(page).count()) === 1 && (await page.getByRole("button", { name: "Send files" }).count()) === 1, "Choose machine: Share a file sits with Send files")
await page.goto(`${origin}/files?machine=${machine.id}`, { waitUntil: "networkidle", timeout: 120000 })
const sharePos = await toolbar(page).first().boundingBox()
const sendPos = await page.getByRole("button", { name: "Send files" }).first().boundingBox()
log(Boolean(sharePos && sendPos) && sharePos.x < sendPos.x && Math.abs(sharePos.y - sendPos.y) < 4, "a machine's Files page: Share a file is next to Send files, on its left")
await shot("share-machine-files")
await page.goto(`${origin}/previews`, { waitUntil: "networkidle", timeout: 120000 })
log((await toolbar(page).count()) === 0, "Previews has no Share a file button")
await page.goto(`${origin}/previews?machine=${machine.id}`, { waitUntil: "networkidle", timeout: 120000 })
log((await toolbar(page).count()) === 0, "Previews (one machine) has no Share a file button")
await page.evaluate(() => {
  const data = new DataTransfer()
  data.items.add(new File(["x"], "drop-check.txt", { type: "text/plain" }))
  window.dispatchEvent(new DragEvent("drop", { dataTransfer: data, bubbles: true, cancelable: true }))
})
await page.waitForTimeout(1500)
log((await dialog.count()) === 0, "a file dropped on Previews opens no share dialog")

// A file in the Inbox for the row menu: sent through Send files, removed at the end.
await page.goto(`${origin}/files?machine=${machine.id}`, { waitUntil: "networkidle", timeout: 120000 })
const inboxName = `share-check-${stamp}.html`
const marker = `PB-SHARE-${stamp}`
const html = `<!doctype html><title>${marker}</title><p>${marker}</p>`
await page.getByRole("button", { name: "Send files" }).first().click()
await dialog.getByText("Drop files here").waitFor({ timeout: 30000 })
await dialog.locator('input[type="file"]').setInputFiles({ name: inboxName, mimeType: "text/html", buffer: Buffer.from(html) })
await dialog.getByText(new RegExp(`^Sent 1 of 1 to ${alias}`)).waitFor({ timeout: 120000 }).catch(() => {})
await dialog.getByRole("button", { name: "Done" }).click().catch(() => {})
const row = page.locator("tbody tr", { hasText: inboxName })
await row.waitFor({ timeout: 60000 }).catch(() => {})
await row.getByRole("button", { name: `Actions for ${inboxName}` }).click()
log(await page.getByRole("menuitem", { name: /^Share link/ }).waitFor({ timeout: 8000 }).then(() => true, () => false), "a file's row menu has Share link…")
await shot("share-row-menu")
await page.getByRole("menuitem", { name: /^Share link/ }).click()
await dialog.getByText("Create link").waitFor({ timeout: 10000 }).catch(() => {})
log(/From .* · Anyone with the link · 24 hours/.test((await dialog.innerText()).replace(/\s+/g, " ")), "Share link… opens with the defaults line", (await dialog.innerText()).replace(/\s+/g, " ").slice(0, 90))
await shot("share-link-dialog")

// Hosting: the link from the Inbox file, then a new file from the dialog.
const content = async (endpoint) => {
  for (let at = 0; at < 20; at++) {
    const res = await ctx.request.get(endpoint, { failOnStatusCode: false }).catch(() => null)
    const body = res?.ok() ? await res.text() : ""
    if (body.includes(marker)) return body
    await new Promise((resolve) => setTimeout(resolve, 1500))
  }
  return ""
}
await dialog.getByRole("button", { name: "Create link" }).click()
const live = dialog.getByText(`${inboxName} is live`)
await live.waitFor({ timeout: 90000 }).catch(() => {})
const inboxLink = await dialog.getByRole("link", { name: "Open" }).getAttribute("href").catch(() => null)
log(Boolean(inboxLink), "Share link… on an Inbox file gives a live link", inboxLink ? new URL(inboxLink).host : (await dialog.innerText()).replace(/\s+/g, " ").slice(0, 160))
if (inboxLink) log((await content(inboxLink)).includes(marker), "opening that link shows the file's content")
await shot("share-link-live")
await page.keyboard.press("Escape")

await page.reload({ waitUntil: "networkidle" })
await toolbar(page).first().click()
await dialog.getByText("Drop a file here").waitFor({ timeout: 30000 })
const line = (await dialog.innerText()).replace(/\s+/g, " ")
log(new RegExp(`Hosted on .* · Anyone with the link · 24 hours`).test(line), "Share a file opens with the defaults line", line.slice(0, 90))
await shot("share-file-dialog")
const sharedName = `shared-${stamp}.html`
await dialog.locator('input[type="file"]').setInputFiles({ name: sharedName, mimeType: "text/html", buffer: Buffer.from(html) })
await dialog.getByText(`${sharedName} is live`).waitFor({ timeout: 90000 }).catch(() => {})
const link = await dialog.getByRole("link", { name: "Open" }).getAttribute("href").catch(() => null)
log(Boolean(link), "Share a file gives a live link", link ? new URL(link).host : (await dialog.innerText()).replace(/\s+/g, " ").slice(0, 160))
if (link) log((await content(link)).includes(marker), "opening that link shows the file's content")
await shot("share-file-live")
await page.keyboard.press("Escape")

// The share appears under Previews, which keeps listing active links.
if (link) {
  await page.goto(`${origin}/previews?machine=${machine.id}`, { waitUntil: "networkidle", timeout: 120000 })
  log(await page.getByText(sharedName).first().isVisible().catch(() => false), "Previews lists the shared file's link")
}

// Clean up the Inbox test file.
await page.goto(`${origin}/files?machine=${machine.id}`, { waitUntil: "networkidle", timeout: 120000 })
const items = ((await (await ctx.request.get(`${origin}/api/v1/machines/${machine.id}/inbox`)).json()).data?.items ?? []).filter((item) => item.name.startsWith("share-check-") || item.name.startsWith("shared-"))
for (const item of items)
  await page.evaluate(
    async ({ url, key }) => (await fetch(url, { method: "DELETE", credentials: "include", headers: { "idempotency-key": key } })).status,
    { url: `/api/v1/machines/${machine.id}/inbox/${encodeURIComponent(item.id)}`, key: Math.random().toString(16).slice(2) },
  )
await browser.close()
console.log(failed ? `${failed} failed` : "all passed")
process.exit(failed ? 1 : 0)
