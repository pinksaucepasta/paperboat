// Browser check for Network > Edges > Change: switching between Paperboat's edges and your own servers while
// previews and tunnels run. The server API is stubbed (no account or server needed), so the
// check shows what the dialog does with the 409 `previews_running` answer and the three follow-ups: the refusal
// lists what runs by name, "Stop these and switch" stays disabled until "switch edges" is typed, confirming
// stops each (preview ended, tunnel paused) before asking again, and the dialog then shows the
// cutover view with each tunnel hostname's DNS state.
//   ORIGIN=<console origin, e.g. http://localhost:3199> CHROME=<Chromium> [PW_MODULE=<playwright-core index.mjs>] node check-edge-switch.mjs
// The console only needs any session cookie (proxy.ts); every /api/v1 call is answered here.
const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const { ORIGIN: origin, CHROME } = process.env
if (!origin || !CHROME) throw new Error("ORIGIN and CHROME are required; see the header of check-edge-switch.mjs")

let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}

const now = "2026-11-18T12:00:00Z"
const edge = (id, site) => ({ id, kind: "managed", region: "fsn1", site, status: "ready", endpoint_host: `${id}.example.test` })
const own = (id, name) => ({
  installation_id: id, node_id: `node_${id}`, endpoint_host: `${id}.example.test`, name, capability: "tunnel", ready: true,
  scope_kind: "account", scope_id: "user", trust_notice: "", enrollment_state: "enrolled",
})
const state = {
  kind: "managed",
  live: [
    { kind: "preview", id: "pv1", name: "https://calm-otter.preview.example.test" },
    { kind: "tunnel", id: "tn1", name: "api tunnel" },
  ],
  stopped: [],
  asked: 0,
}
const tunnel = (desired) => ({
  schema: "paperboat.preview-tunnel/v1", kind: "tunnel", id: "tn1", account_id: "user", name: "api tunnel", desired_state: desired,
  access_mode: "public", etag: '"1"', generation: 1, stable_endpoint: "https://api.example.test", stable_endpoint_id: "e1",
  created_by_host_id: "m1", created_by_actor_id: "u", summary_code: "pending", created_at: now, updated_at: now,
})
const ok = (route, data, status = 200) => route.fulfill({ status, contentType: "application/json", body: JSON.stringify({ data }) })

const browser = await chromium.launch({ executablePath: CHROME })
const context = await browser.newContext({ viewport: { width: 1280, height: 900 } })
await context.addCookies([{ name: "pb-dev-session", value: "x", url: origin }])
const page = await context.newPage()
const reads = []
if (process.env.DEBUG) page.on("console", (message) => console.log(`[browser ${message.type()}]`, message.text().slice(0, 300)))
await context.route("**/api/v1/**", async (route) => {
  const request = route.request()
  const url = new URL(request.url())
  const path = url.pathname.replace("/api/v1/", "")
  const method = request.method()
  if (path === "edges/pair" && method === "GET")
    return ok(route, state.kind === "managed" ? { primary: edge("m1", "Falkenstein"), fallback: edge("m2", "Helsinki") } : { primary: { ...edge("s1", "Server A"), kind: "self_hosted", id: "s1" }, fallback: { ...edge("s2", "Server B"), kind: "self_hosted", id: "s2" } })
  if (path === "edges/pair" && method === "PUT") {
    state.asked++
    if (state.stopped.length < state.live.length)
      return route.fulfill({
        status: 409, contentType: "application/json",
        body: JSON.stringify({ error: { code: "previews_running", message: "Stop this workspace's running previews and tunnels first.", details: { count: state.live.length, items: state.live } } }),
      })
    state.kind = "self_hosted"
    return ok(route, { primary: { ...edge("s1", "Server A"), kind: "self_hosted" }, fallback: { ...edge("s2", "Server B"), kind: "self_hosted" } })
  }
  if (path === "workspaces") return ok(route, { items: [{ id: "personal", name: "Personal", kind: "personal", role: "owner" }] })
  if (path === "relays") return ok(route, { items: [], total: 0, observed_at: now })
  if (path === "edges") return ok(route, { items: [edge("m1", "Falkenstein"), edge("m2", "Helsinki")], total: 2 })
  if (path === "selfhost/installations") {
    const tunnels = [own("s1", "Server A"), own("s2", "Server B")]
    const items = url.searchParams.get("capability") === "tunnel" ? tunnels : []
    return ok(route, { installations: items, pagination: { limit: 50, offset: 0, total: items.length, next_offset: null }, counts: { ready: items.length, enrolled: items.length, pending: 0 } })
  }
  if (path.startsWith("selfhost/pools/")) return ok(route, { mode: path.endsWith("tunnel") ? "managed-only" : "mixed", installation_ids: [] })
  if (path === "previews/pv1" && method === "GET") return route.fulfill({ status: 200, headers: { etag: '"1"' }, contentType: "application/json", body: JSON.stringify({ data: {} }) })
  if (path === "previews/pv1" && method === "DELETE") { state.stopped.push("preview"); return ok(route, {}) }
  if (path === "tunnels/tn1" && method === "GET") return ok(route, tunnel(state.stopped.includes("tunnel") ? "paused" : "active"))
  if (path === "tunnels/tn1/pause") { state.stopped.push("tunnel"); return ok(route, tunnel("paused")) }
  if (path === "tunnels") return ok(route, { items: [tunnel("paused")], total: 1 })
  if (path === "tunnels/tn1/domains")
    return ok(route, { items: [{ schema: "paperboat.preview-tunnel/v1", kind: "domain_binding", id: "d1", account_id: "user", tunnel_id: "tn1", hostname: "api.myapp.example.test", route_id: "r1", state: state.kind === "managed" ? "ready" : "waiting_dns", match_type: "exact", dns: { target: "198.51.100.7" }, certificate: { state: "pending" }, generation: 2, etag: '"2"' }], total: 1 })
  reads.push(`${method} ${path}`)
  return ok(route, { items: [], total: 0 })
})

try {
  await page.goto(`${origin}/network`, { waitUntil: "networkidle" })
  await page.getByRole("button", { name: "Change" }).first().click()
  const dialog = page.getByRole("dialog")
  await dialog.getByRole("heading", { name: "Change edges" }).waitFor()
  log(true, "Change edges opens on the Network page")

  // Pick two of the workspace's own servers.
  const pick = async (id, label) => {
    await dialog.locator(`#${id}`).click()
    await page.getByRole("option", { name: new RegExp(label) }).first().click()
  }
  await pick("edge-primary", "Server A")
  await pick("edge-fallback", "Server B")
  await dialog.getByRole("button", { name: "Save" }).click()

  await dialog.getByText("have to stop before you switch").waitFor()
  const list = dialog.getByRole("list", { name: "Running on the current edges" })
  const items = (await list.locator("li").allInnerTexts()).map((text) => text.replace(/\s+/g, " "))
  log(items.length === 2 && items.some((t) => t.includes("Preview") && t.includes("calm-otter.preview.example.test") && t.includes("ends")), "preview named by its address", items[0])
  log(items.some((t) => t.includes("Tunnel") && t.includes("api tunnel") && t.includes("is paused")), "tunnel named, and said to pause")

  const confirm = dialog.getByRole("button", { name: "Stop these and switch" })
  log(await confirm.isDisabled(), "Stop these and switch is disabled until the confirmation is typed")
  await dialog.getByLabel(/Type switch edges/).fill("switch edge")
  log(await confirm.isDisabled(), "a partly typed confirmation does not enable it")
  await dialog.getByLabel(/Type switch edges/).fill("switch edges")
  log(await confirm.isEnabled(), "the typed confirmation enables it")
  await page.screenshot({ path: process.env.SHOT_REFUSAL ?? "/tmp/edge-switch-refusal.png" })
  await confirm.click()

  await page.getByRole("heading", { name: "Edges changed" }).waitFor()
  log(state.stopped.slice().sort().join() === "preview,tunnel", "preview ended and tunnel paused before the switch", state.stopped.join())
  const cutover = dialog.getByRole("list", { name: "Domains on the new edges" })
  await cutover.waitFor()
  const row = (await cutover.locator("li").first().innerText()).replace(/\s+/g, " ")
  log(row.includes("api.myapp.example.test") && row.includes("api tunnel") && row.includes("Waiting for DNS"), "each hostname shows its DNS state until the new target verifies", row)
  log(await dialog.getByText(/Create the stopped previews again and resume the paused tunnels/).isVisible(), "tells to create previews again and resume tunnels")
  log(await dialog.getByText(/need a custom domain/).isVisible(), "tells that own servers need a custom domain")
  log(await dialog.getByText(/old record’s cache expires/).isVisible(), "explains the DNS wait")
  await page.screenshot({ path: process.env.SHOT_DONE ?? "/tmp/edge-switch-done.png" })
  await dialog.getByRole("button", { name: "Done" }).click()
  await dialog.waitFor({ state: "detached" })
  log(true, "Done closes the dialog")
} catch (error) {
  log(false, "check ran to the end", String(error).split("\n")[0])
  await page.screenshot({ path: "/tmp/edge-switch-failure.png" }).catch(() => {})
  if (reads.length) console.log("unstubbed reads:", [...new Set(reads)].join(", "))
}
await browser.close()
process.exit(failed ? 1 : 0)
