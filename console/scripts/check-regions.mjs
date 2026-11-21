// Browser check of Network > Your regions: creating a region, adding an edge to it (command, pairing, the live wait), the
// machine row and its actions (Drain, Drain and move, Resume, Remove), Rename, Remove region and the empty / error states.
// It runs the console from this checkout against a small stub control plane (no real server, nothing is sent or changed).
// usage: node scripts/check-regions.mjs            PW_MODULE=<playwright-core index.js> PW_CHROMIUM=<chrome path> when not installed
// It starts `next dev` on a free port with PAPERBOAT_SERVER_URL pointing at the stub. Exits 1 when a step fails.
import { createServer } from "node:http"
import { spawn } from "node:child_process"
import { createServer as net } from "node:net"
import { fileURLToPath } from "node:url"

const playwright = await import(process.env.PW_MODULE ?? "playwright-core")
const chromium = playwright.chromium ?? playwright.default.chromium
const freePort = () => new Promise((resolve) => { const s = net().listen(0, "127.0.0.1", () => { const { port } = s.address(); s.close(() => resolve(port)) }) })

const nowIso = () => new Date().toISOString()
const state = {
  regions: [], // { id, name }
  installs: [], // SelfhostInstallation
  posts: { create: [], rename: [], pair: [], drain: [], resume: [], remove: [], deleteRegion: [] },
  inUse: false, // a workspace's regions name the region: DELETE answers 409 region_in_use
  unstubbed: new Set(),
}
const installation = (over = {}) => ({
  installation_id: "inst-edge-1", node_id: "node-1", endpoint_host: "203.0.113.7", name: "edge-1", capability: "tunnel",
  ready: false, draining: false, trust_notice: "", scope_kind: "account", scope_id: "usr_1", enrollment_state: "enrolled",
  region: "home-lab", version: "1.4.0", ...over,
})
const regionList = () =>
  state.regions.map((region) => {
    const nodes = new Map(state.installs.filter((i) => i.region === region.id && i.enrollment_state === "enrolled").map((i) => [i.node_id, i]))
    return { id: region.id, name: region.name, machines: nodes.size, ready: [...nodes.values()].filter((i) => i.ready).length }
  })
const slug = (name) => name.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "")

const json = (response, status, body) => {
  response.writeHead(status, { "content-type": "application/json" })
  response.end(JSON.stringify(body))
}
const ok = (response, data, status = 200) => json(response, status, { data })
const fail = (response, status, code, message) => json(response, status, { error: { code, message } })

const read = (request) => new Promise((resolve) => { let body = ""; request.on("data", (c) => (body += c)); request.on("end", () => resolve(body ? JSON.parse(body) : {})) })

const api = createServer(async (request, response) => {
  const url = new URL(request.url, "http://x")
  const path = url.pathname.replace(/^\/v1\//, "")
  const method = request.method
  const get = method === "GET"
  if (path === "auth/csrf") return ok(response, { csrf_token: "stub" })
  if (get && path === "me") return ok(response, { id: "usr_1", email: "ana@example.test", display_name: "Ana", status: "active", role: "user", workos_subject: "sub_1" })
  if (get && path === "workspaces") return ok(response, { items: [{ id: "personal", name: "Personal", kind: "personal", role: "owner" }] })
  if (get && path === "announcements") return ok(response, { banner: null, notices: [], unread: 0 })
  if (get && path === "client-configuration") return ok(response, { installer_base_url: "https://get.example.test" })
  if (get && path === "relays") return ok(response, { items: [{ id: "fsn1", name: "Falkenstein", status: "ready" }], observed_at: nowIso() })
  if (get && path === "edges") return ok(response, { items: [{ id: "fsn1", name: "Falkenstein", status: "ready" }, { id: "hel1", name: "Helsinki", status: "ready" }], observed_at: nowIso() })
  if (get && path === "edges/pair")
    return ok(response, { kind: "managed", primary: { id: "fsn1", name: "Falkenstein", status: "ready" }, fallback: { id: "hel1", name: "Helsinki", status: "ready" } })
  if (path.startsWith("selfhost/pools/")) return ok(response, { mode: path.endsWith("tunnel") ? "managed-only" : "mixed", installation_ids: [] })

  if (get && path === "selfhost/regions") return ok(response, regionList())
  if (method === "POST" && path === "selfhost/regions") {
    const body = await read(request)
    state.posts.create.push(body)
    const taken = state.regions.find((region) => region.name.toLowerCase() === String(body.name).trim().toLowerCase())
    if (taken) return fail(response, 409, "region_exists", `A region called “${taken.name}” already exists.`)
    const region = { id: slug(body.name), name: String(body.name).trim() }
    state.regions.push(region)
    return ok(response, { ...region, machines: 0, ready: 0 }, 201)
  }
  const regionPath = path.match(/^selfhost\/regions\/([^/]+)$/)
  if (method === "PATCH" && regionPath) {
    const body = await read(request)
    state.posts.rename.push(body)
    const region = state.regions.find((item) => item.id === regionPath[1])
    if (!region) return fail(response, 404, "region_not_found", "Region not found.")
    region.name = String(body.name).trim()
    return ok(response, regionList().find((item) => item.id === region.id))
  }
  if (method === "DELETE" && regionPath) {
    state.posts.deleteRegion.push(regionPath[1])
    if (state.inUse)
      return fail(response, 409, "region_in_use", "Switch your workspace's regions away from this region first, then remove it.")
    state.regions = state.regions.filter((item) => item.id !== regionPath[1])
    return ok(response, null)
  }

  if (method === "POST" && path === "selfhost/enrollments") {
    const body = await read(request)
    state.posts.pair.push(body)
    if (body.code === "pbh1.wrong")
      return fail(response, 400, "invalid_request", "This machine was installed for region lab, not home-lab: run pbh install --region home-lab on it.")
    const added = installation()
    state.installs.push(added)
    return ok(response, { installations: [added] })
  }
  if (get && path === "selfhost/installations") {
    const capability = url.searchParams.get("capability")
    const q = url.searchParams.get("q") ?? ""
    const items = state.installs.filter((i) => (!capability || i.capability === capability) && (!q || [i.name, i.installation_id, i.node_id, i.endpoint_host].some((v) => v.includes(q))))
    return ok(response, { installations: items, pagination: { limit: 50, offset: 0, total: items.length, next_offset: null }, counts: { ready: items.filter((i) => i.ready).length, enrolled: items.length, pending: 0 } })
  }
  const action = path.match(/^selfhost\/installations\/([^/]+)\/(drain|resume)$/)
  if (method === "POST" && action) {
    const body = await read(request)
    const target = state.installs.find((i) => i.installation_id === action[1])
    if (!target) return fail(response, 404, "not_found", "Not found.")
    state.posts[action[2]].push(body)
    if (action[2] === "drain") Object.assign(target, { draining: true, ready: false, drain_deadline: new Date(Date.now() + 600_000).toISOString() })
    else Object.assign(target, { draining: false, ready: true, drain_deadline: undefined })
    return ok(response, target)
  }
  const removal = path.match(/^selfhost\/installations\/([^/]+)$/)
  if (method === "DELETE" && removal) {
    state.posts.remove.push(removal[1])
    state.installs = state.installs.filter((i) => i.installation_id !== removal[1])
    return ok(response, {})
  }
  if (get) {
    state.unstubbed.add(path)
    return ok(response, { items: [], total: 0 })
  }
  return fail(response, 404, "not_found", "stub")
})
await new Promise((resolve) => api.listen(0, "127.0.0.1", resolve))
const apiPort = api.address().port
const port = await freePort()
const next = spawn("bunx", ["next", "dev", "-p", String(port)], {
  cwd: fileURLToPath(new URL("..", import.meta.url)),
  env: { ...process.env, PAPERBOAT_SERVER_URL: `http://127.0.0.1:${apiPort}`, WORKOS_REDIRECT_URI: `http://127.0.0.1:${port}/auth/callback`, NEXT_TELEMETRY_DISABLED: "1" },
  stdio: process.env.DEBUG ? "inherit" : "ignore",
})
let failed = 0
const check = (step, ok, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}
const text = async (locator) => ((await locator.innerText()) ?? "").replace(/\s+/g, " ").trim()
let browser
let page
try {
  const origin = `http://127.0.0.1:${port}`
  for (let i = 0; i < 120; i++) { if (await fetch(origin + "/auth/sign-in").then(() => true, () => false)) break; await new Promise((r) => setTimeout(r, 1000)) }
  browser = await chromium.launch(process.env.PW_CHROMIUM ? { executablePath: process.env.PW_CHROMIUM } : {})
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
  await context.addCookies([{ name: "pb-dev-session", value: "stub", url: origin }])
  await context.grantPermissions(["clipboard-read", "clipboard-write"], { origin }).catch(() => {})
  page = await context.newPage()
  if (process.env.DEBUG) page.on("console", (message) => console.log(`[browser ${message.type()}]`, message.text().slice(0, 300)))
  const overflow = () => page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)
  const slow = { timeout: 60_000 }

  // 1. No regions: the empty state and its action.
  await page.goto(`${origin}/network`, { timeout: 120_000 })
  await page.getByText("Run your own servers in a region you name").waitFor(slow)
  check("empty: says to run your own servers in a region you name", true)
  check("empty: Create a region, then add a relay or an edge to it", await page.getByText("Create a region, then add a relay or an edge to it.").isVisible())
  check("empty: New region is offered", (await page.getByRole("button", { name: "New region" }).count()) >= 1)
  check("no 'Your servers' section or Add server button any more", (await page.getByText("Your servers").count()) === 0 && (await page.getByRole("button", { name: "Add server" }).count()) === 0)

  // 2. New region "Home lab".
  await page.getByRole("button", { name: "New region" }).first().click()
  let dialog = page.getByRole("dialog")
  await dialog.getByRole("heading", { name: "New region" }).waitFor()
  await dialog.getByLabel("Region name").fill("Home lab")
  await dialog.getByRole("button", { name: "Create region" }).click()
  const card = page.locator('[data-region="home-lab"]')
  await card.waitFor(slow)
  check("create: POST body is the name", JSON.stringify(state.posts.create.at(-1)) === JSON.stringify({ name: "Home lab" }), JSON.stringify(state.posts.create))
  check("region card: name, slug (muted), Empty, 0 machines", (await text(card)).includes("Home lab") && (await text(card)).includes("home-lab") && (await text(card.locator("[data-region-status]"))) === "Empty" && /0 machines/.test(await text(card)), await text(card))
  check("region card: no machines yet, with what to do", (await text(card)).includes("No machines yet. Add a relay or an edge to bring this region online."))
  check("empty state is gone once a region exists", (await page.getByText("Run your own servers in a region you name").count()) === 0)

  // 3. A second region of the same name: the server's sentence names the existing one.
  await page.getByRole("button", { name: "New region" }).first().click()
  dialog = page.getByRole("dialog")
  await dialog.getByLabel("Region name").fill("home LAB")
  await dialog.getByRole("button", { name: "Create region" }).click()
  await dialog.getByText("A region called “Home lab” already exists.").waitFor(slow)
  check("create: 409 region_exists names the existing region, inline", true)
  await dialog.getByRole("button", { name: "Cancel" }).click()
  await dialog.waitFor({ state: "detached" })

  // 4. Add edge: the command carries role and region, one copy button.
  await card.getByRole("button", { name: "Add edge" }).click()
  dialog = page.getByRole("dialog")
  await dialog.getByRole("heading", { name: "Add edge to Home lab" }).waitFor()
  const command = "curl -fsSL https://get.example.test/selfhost | sh -s -- --edge --region home-lab"
  const shown = await text(dialog.locator("code").first())
  check("add edge: one command with --edge --region home-lab", shown === command, shown)
  check("add edge: exactly one command block", (await dialog.locator("code").count()) === 1)
  check("add edge: says where to run it", await dialog.getByText("Run this on a Linux server with a public IP.").isVisible())
  const copy = dialog.getByRole("button", { name: "Copy command" })
  check("add edge: copy button", (await copy.count()) === 1)
  await copy.click()
  await dialog.getByRole("button", { name: "Copied" }).waitFor({ timeout: 5000 }).then(() => check("add edge: the copy button confirms", true), () => check("add edge: the copy button confirms", false))
  const clip = await page.evaluate(() => navigator.clipboard.readText()).catch(() => undefined)
  if (clip !== undefined) check("add edge: the clipboard holds the command", clip === command, clip)
  check("add edge: Paste what the installer printed", await dialog.getByText("Paste what the installer printed").isVisible())

  // 5. Pairing with a machine installed for another region: the server's message, inline.
  await dialog.getByLabel("Address").fill("203.0.113.7:8443")
  await dialog.getByLabel("Code").fill("pbh1.wrong")
  await dialog.getByRole("button", { name: "Pair edge" }).click()
  await dialog.getByText(/installed for region lab, not home-lab/).waitFor(slow)
  check("pair: wrong region shows the server's message inline", true)
  check("pair: the dialog stays open on the form", await dialog.getByLabel("Code").isVisible())
  check("pair: the failed attempt sent expect_region and expect_capability", JSON.stringify(state.posts.pair.at(-1)) === JSON.stringify({ address: "203.0.113.7:8443", code: "pbh1.wrong", scope: { kind: "account" }, expect_region: "home-lab", expect_capability: "tunnel" }), JSON.stringify(state.posts.pair.at(-1)))

  // 6. The right code: the claim returns, the machine has not reported yet.
  await dialog.getByLabel("Code").fill("pbh1.good")
  await dialog.getByRole("button", { name: "Pair edge" }).click()
  await dialog.getByText("Waiting for the machine to connect").waitFor(slow)
  check("wait: 'Waiting for the machine to connect'", true)
  check("wait: enrolled but not reported says pairing finished", await dialog.getByText("Pairing finished; the machine has not reported yet.").isVisible())
  check("wait: a status command for a machine that does not respond", (await text(dialog.locator("code").last())) === "pbh status")

  // 7. The machine reports in: the wait turns into the machine's name and status.
  state.installs[0].ready = true
  state.installs[0].last_heartbeat_at = nowIso()
  await dialog.getByRole("button", { name: "Done" }).waitFor({ timeout: 30_000 })
  check("wait: turns into the machine's name, role, region and Ready", (await text(dialog)).includes("edge-1") && /Edge in Home lab: Ready/.test(await text(dialog)), await text(dialog))
  await dialog.getByRole("button", { name: "Done" }).click()
  await dialog.waitFor({ state: "detached" })

  // 8. The machine row under its region.
  const row = card.getByRole("row", { name: /edge-1/ })
  await row.waitFor(slow)
  const rowText = await text(row)
  check("row: role Edge, version and status", /Edge/.test(rowText) && /1\.4\.0/.test(rowText) && /Ready/.test(rowText), rowText)
  check("row: region card says Ready, 1 machine", (await text(card.locator("[data-region-status]"))) === "Ready" && /1 machine\b/.test(await text(card)), await text(card))
  check("row: No 'device' wording anywhere in the card", !/device/i.test(await text(card)))
  await page.setViewportSize({ width: 390, height: 900 })
  check("phone (390 px): no horizontal overflow with a machine", (await overflow()) <= 0, `${await overflow()}px`)
  await page.setViewportSize({ width: 1280, height: 900 })

  // 9. Drain, Drain and move, Resume.
  const open = () => page.getByRole("button", { name: "Actions for edge-1" }).click()
  const alert = page.getByRole("alertdialog")
  await open()
  await page.getByRole("menuitem", { name: "Drain", exact: true }).click()
  await alert.getByText("New work stops going to this machine. Running work finishes (up to 10 minutes).").waitFor()
  await alert.getByRole("button", { name: "Drain", exact: true }).click()
  await alert.waitFor({ state: "detached" })
  check("drain: body {move:false}", JSON.stringify(state.posts.drain.at(-1)) === '{"move":false}', JSON.stringify(state.posts.drain))
  await row.getByText("Draining", { exact: true }).waitFor(slow)
  check("drain: the row says Draining and the region too", (await text(card.locator("[data-region-status]"))) === "Draining", await text(card.locator("[data-region-status]")))
  await open()
  await page.getByRole("menuitem", { name: "Drain and move", exact: true }).click()
  await alert.getByText("Running work is moved off this machine now. It drains within about 90 seconds.").waitFor()
  await alert.getByRole("button", { name: "Drain and move", exact: true }).click()
  await alert.waitFor({ state: "detached" })
  check("drain and move: body {move:true}", JSON.stringify(state.posts.drain.at(-1)) === '{"move":true}', JSON.stringify(state.posts.drain))
  await open()
  await page.getByRole("menuitem", { name: "Resume" }).click()
  await row.getByText("Ready", { exact: true }).waitFor(slow)
  check("resume: POST sent and the row is Ready again", state.posts.resume.length === 1 && !state.installs[0].draining)

  // 10. Rename: the name changes, the slug stays.
  await card.getByRole("button", { name: "Actions for Home lab" }).click()
  await page.getByRole("menuitem", { name: "Rename" }).click()
  dialog = page.getByRole("dialog")
  await dialog.getByRole("heading", { name: "Rename Home lab" }).waitFor()
  await dialog.getByLabel("Region name").fill("Lab bench")
  await dialog.getByRole("button", { name: "Rename", exact: true }).click()
  await card.getByRole("heading", { name: "Lab bench" }).waitFor(slow)
  check("rename: PATCH body is the new name", JSON.stringify(state.posts.rename.at(-1)) === JSON.stringify({ name: "Lab bench" }), JSON.stringify(state.posts.rename))
  check("rename: the slug is unchanged", (await text(card)).includes("home-lab"))

  // 11. Remove the machine (typed name), then the region.
  await open()
  await page.getByRole("menuitem", { name: "Remove machine" }).click()
  const removeButton = alert.getByRole("button", { name: "Remove machine" })
  check("remove machine: disabled until the name is typed", await removeButton.isDisabled())
  await alert.getByLabel(/Type edge-1/).fill("edge-1")
  await removeButton.click()
  await alert.waitFor({ state: "detached" })
  check("remove machine: DELETE sent for the installation", state.posts.remove.join() === "inst-edge-1", state.posts.remove.join())
  await card.getByText("No machines yet. Add a relay or an edge to bring this region online.").waitFor(slow)
  check("region is empty again after the last machine goes", true)

  state.inUse = true
  await card.getByRole("button", { name: "Actions for Lab bench" }).click()
  await page.getByRole("menuitem", { name: "Remove region" }).click()
  const removeRegion = alert.getByRole("button", { name: "Remove region" })
  check("remove region: disabled until the name is typed", await removeRegion.isDisabled())
  await alert.getByLabel(/Type Lab bench/).fill("Lab bench")
  await removeRegion.click()
  await alert.getByText("Switch your workspace's regions away from this region first, then remove it.").waitFor(slow)
  check("remove region: 409 region_in_use shows the server's message", true)
  check("remove region: the region is still there", (await card.count()) === 1)
  state.inUse = false
  await removeRegion.click()
  await alert.waitFor({ state: "detached" })
  await page.getByText("Run your own servers in a region you name").waitFor(slow)
  check("remove region: DELETE sent and the empty state returns", state.posts.deleteRegion.join() === "home-lab,home-lab", state.posts.deleteRegion.join())

  // 12. Paperboat's own regions show no machines.
  await page.getByText("Falkenstein").first().waitFor(slow)
  const insideCards = await page.locator("[data-region] tr").count()
  const actionButtons = await page.locator('button[aria-label^="Actions for"]').count()
  check("managed regions: Relays and Edges list regions only (no machine rows, no machine actions)", insideCards === 0 && actionButtons === 0, `${insideCards} rows in cards, ${actionButtons} action buttons`)
  check("managed regions: no Machine column outside your own regions", (await page.getByRole("columnheader", { name: "Machine" }).count()) === 0)

  // 13. Phone width.
  await page.setViewportSize({ width: 390, height: 900 })
  check("phone (390 px): no horizontal overflow", (await overflow()) <= 0, `${await overflow()}px`)
  if (process.env.SHOTS) await page.screenshot({ path: `${process.env.SHOTS}/regions-phone.png`, fullPage: true })
} catch (error) {
  check("check ran to the end", false, String(error).split("\n")[0])
  if (page && process.env.SHOTS) await page.screenshot({ path: `${process.env.SHOTS}/regions-failure.png`, fullPage: true }).catch(() => {})
  if (state.unstubbed.size) console.log("unstubbed reads:", [...state.unstubbed].join(", "))
} finally {
  await browser?.close()
  next.kill("SIGTERM")
  api.close()
}
console.log(failed ? `${failed} failed` : "all passed")
process.exit(failed ? 1 : 0)
