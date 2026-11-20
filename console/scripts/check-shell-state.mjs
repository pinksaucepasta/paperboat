// Headless check that what the user enters on a page's static shell survives the server page that replaces it.
// On a client navigation Next renders the page's static shell (WithSession's Suspense fallback) while the server
// renders; React then replaces the shell's tree (it never reconciles a fallback with its content), so plain
// `useState` would be lost (lib/use-shell-state.tsx keeps it). Only pages with a `WithSession` leaf show such
// a shell (tunnels, terminals, config, activity, files, settings/recipes list, [name], [name]/edit,
// settings/api-tokens, and every page under /machines/[machineId]/ via the machine layout's own leaf); the
// Files shell has no machine picked (its Inbox filename filter needs one), so it has no case. For each case:
// hold the server's render (every control-plane read waits on a promise: no sleeping), push the target on the
// client, enter state on the SHELL (the live tree stays interactive throughout), release, and assert the server
// page has the same state. The machine route is never prefetched, so its push always renders the live fallback.
// The run starts with failing reads, so only static shells cache and every push suspends.
// It runs the BUILT console (`NEXT_DIST_DIR=<dir> bun run build`, then this) against a fixture control plane, so no
// WorkOS sign-in and no live account is needed.
// usage: PW_CHROMIUM=<chromium> [PW_MODULE=<playwright-core index.mjs>] [NEXT_DIST_DIR=.next] [APP_PORT=3199] [FIXTURE_PORT=3198]
//        [ONLY=<substring of a case name>] node scripts/check-shell-state.mjs
import { spawn } from "node:child_process"
import http from "node:http"

const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const APP_PORT = Number(process.env.APP_PORT ?? 3199)
const FIXTURE_PORT = Number(process.env.FIXTURE_PORT ?? 3198)
const ORIGIN = `http://127.0.0.1:${APP_PORT}`
if (!process.env.PW_CHROMIUM) throw new Error("PW_CHROMIUM is not set; see the header of check-shell-state.mjs")

// ---- fixture control plane ----------------------------------------------------------------------
// The run starts failing every data read, so only the static shells cache: a push then suspends on the held
// render instead of showing a fully prefetched page at once. Reads start working once the shells are warm.
let failing = true
const world = { hold: null } // a promise every control-plane read waits for while a case runs
const empty = { items: [], pagination: { limit: 10, offset: 0, total: 0, next_offset: null } }
// Validated with checkResponse("GET", "machines/m1" | "machines?…" | "workspaces/personal/tags", …).
const machineFixture = {"ownership": "x", "id": "m1", "environment_id": "x", "alias": "web-1", "platform": "x", "architecture": "x", "workspace_root": "x", "state": "x", "seat_state": "x", "public_identity_key": "x", "can_manage": true, "shared": true, "online": true, "network_approved": true, "permissions": ["x"], "installation_generation": 1, "runtime_versions": {}, "capabilities": {"file_receive": {"configured": true, "observed": true}, "preview_launch": {"configured": true, "observed": true}, "terminal_host": {"configured": true, "observed": true}, "session_host": {"configured": true, "observed": true}, "keep_awake": {"configured": true, "observed": true}, "environment_injection": {"configured": true, "observed": true}}, "availability": {"schema": "x", "desired_mode": "x", "desired_version": 1, "observed_version": 1, "status": "x", "update_rollbacks": 1, "update_health": "x"}, "runtime_diagnostics": {"worker_generation": 1, "worker_service_scope": "x", "connector_state": "x", "connector_generation": 1}, "machine_capabilities": {"schema": "x", "desired": {"terminal": true, "managed_ssh": true, "file_receive": true, "preview_tunnel": true}, "desired_version": 1, "applied": {"terminal": true, "managed_ssh": true, "file_receive": true, "preview_tunnel": true}, "applied_version": 1, "status": "x"}, "owner_account": "u"}
// A live preview on the fixture machine, so the previews tab renders its list (with search and
// the Live/Ended toggle) instead of its empty state. Validated with checkResponse("GET", "previews?…""
// for the counts and page reads; the deadline is an hour out so the lease reads as live.
const previewFixture = {"access_mode": "x", "account_id": "x", "actor_id": "x", "allocation_state": "x", "domains": [{"schema": "x", "kind": "x", "id": "x", "account_id": "x", "hostname": "x", "match_type": "x", "state": "x", "dns": {"target": "x"}, "certificate": {"state": "x"}, "generation": 1, "etag": "x", "target_kind": "x", "preview_id": "x"}], "edge_state": "up", "endpoint": "https://demo-1.example.test", "endpoint_state": "ready", "id": "pv1", "kind": "x", "lease_deadline": "2026-11-20T03:30:00.000Z", "origin_state": "x", "owner_machine_id": "m1", "owner_session_id": "x", "owner_session_kind": "x", "persistent": "x", "schema": "x", "state": "ready", "target": {"scheme": "x", "address": "x"}, "user_deadline": null}
function route(url) {
  const p = url.pathname
  if (p === "/v1/me") return { id: "u", email: "owner@example.test", display_name: "Owner", status: "active", role: "user", workos_subject: "wo_u" }
  if (p === "/v1/workspaces") return { items: [{ id: "personal", name: "Personal", kind: "personal", role: "owner" }, { id: "team", name: "Team", kind: "team", role: "owner" }] }
  // ---- a page that needs more fixture data: the Network search only shows past one page of servers --
  const installations = Array.from({ length: 51 }, (_, i) => ({
    installation_id: `i${i + 1}`,
    node_id: `n${i + 1}`,
    endpoint_host: "",
    name: `relay-${i + 1}`,
    capability: "relay",
    ready: true,
    trust_notice: "",
    scope_kind: "account",
    scope_id: "",
    enrollment_state: "enrolled",
  }))
  const installationPage = {
    installations,
    pagination: { limit: 50, offset: 0, total: installations.length, next_offset: null },
    counts: { ready: installations.length, enrolled: installations.length, pending: 0 },
  }
  if (p === "/v1/selfhost/installations") return installationPage
  if (p === "/v1/client-configuration") return { purchase_consent: { version: "2026-11-19", text: "Start the service now." } }
  // ---- a fixture machine: the machine tab reads it and the workspace's tags, lists show it ----
  // Shapes generated from the documented responses (console/lib/api-sample.ts), so the boundary
  // check (lib/api-validate.ts) accepts them.
  if (p === "/v1/machines/m1") return machineFixture
  if (p === "/v1/machines")
    return { items: [machineFixture], pagination: { limit: 50, offset: 0, total: 1, next_offset: null } }
  if (p.startsWith("/v1/workspaces/") && p.endsWith("/tags")) return { tags: [], can_edit: "x", policy_version: 1 }
  if (p === "/v1/previews") return { items: [previewFixture], total: 1 }
  return empty
}
const fixture = http.createServer(async (req, res) => {
  const url = new URL(req.url, `http://127.0.0.1:${FIXTURE_PORT}`)
  if (failing && url.pathname.startsWith("/v1/")) {
    res.writeHead(500, { "content-type": "application/json" })
    res.end(JSON.stringify({ error: { message: "shell-state fixture warming up", reference: "shell-state-warmup" } }))
    return
  }
  // Hold every control-plane read while a case runs: whatever the server page suspends on,
  // the shell stays until the case releases it (the server's own fetch timeout bounds the wait).
  if (url.pathname.startsWith("/v1/")) await world.hold
  res.writeHead(200, { "content-type": "application/json" })
  res.end(JSON.stringify({ data: route(url) }))
})
await new Promise((resolve) => fixture.listen(FIXTURE_PORT, "127.0.0.1", resolve))

const app = spawn("bun", ["x", "next", "start", "-p", String(APP_PORT), "-H", "127.0.0.1"], {
  env: { ...process.env, PAPERBOAT_SERVER_URL: `http://127.0.0.1:${FIXTURE_PORT}`, WORKOS_REDIRECT_URI: `${ORIGIN}/auth/callback`, NEXT_DIST_DIR: process.env.NEXT_DIST_DIR ?? ".next" },
  stdio: "ignore",
})
async function appUp() {
  for (let i = 0; i < 60; i++) {
    try {
      if ((await fetch(`${ORIGIN}/auth/sign-in`, { redirect: "manual" })).status < 500) return
    } catch {}
    await new Promise((r) => setTimeout(r, 500))
  }
  throw new Error("the console did not start; build it first (NEXT_DIST_DIR=<dir> bun run build)")
}

// ---- the cases: [name, path, what to enter on the shell, what the server page must still show, workspace, what the shell must show once entered] ---------
// The view toggles are toggle buttons, not radios (components/list-toolbar.tsx ViewToggle); a loaded
// count joins the name ("Ended 0"), so match the label at the start.
const checked = (page, name) => page.getByRole("button", { name: new RegExp(`^${name}`) }).first()
const pressed = async (page, name) => (await checked(page, name).getAttribute("aria-pressed")) === "true"
const clearSearch = async (p) => (await p.getByRole("button", { name: "Clear search" }).count()) > 0
// How to tell the shell from the server page: every page under (console)/, machine tabs included,
// shows WithSession's own scoped fallback (.shell-delay) while its Session suspends.
const shellDelay = {
  wait: (p) => p.waitForSelector(".shell-delay", { state: "attached", timeout: 15000 }),
  showing: (p) => p.locator(".shell-delay").count(),
  loaded: (p) => p.waitForFunction(() => !document.querySelector(".shell-delay"), null, { timeout: 30000 }),
}
const cases = [
  [
    "Tunnels: search text and the New tunnel dialog with its name",
    "/tunnels",
    async (p) => {
      await p.getByPlaceholder("Search tunnels").fill("api")
      if (!(await p.locator("#tunnel-name").count())) await p.getByRole("button", { name: "New tunnel" }).first().click()
      if (await p.locator("#tunnel-name").count()) await p.locator("#tunnel-name").fill("my-tunnel")
    },
    async (p) => (await p.getByPlaceholder("Search tunnels").inputValue()) === "api" && (await p.locator("#tunnel-name").inputValue()) === "my-tunnel",
    undefined,
    async (p) => (await clearSearch(p)) && (await p.locator("#tunnel-name").count()) > 0 && (await p.locator("#tunnel-name").inputValue()) === "my-tunnel",
  ],
  [
    "Terminals: the Closed view and search text",
    "/terminals",
    async (p) => {
      await checked(p, "Closed").click()
      await p.getByPlaceholder("Search terminals").fill("build")
    },
    async (p) => (await pressed(p, "Closed")) && (await p.getByPlaceholder("Search terminals").inputValue()) === "build",
    undefined,
    async (p) => (await pressed(p, "Closed")) && (await clearSearch(p)),
  ],
  ["Config sync: search text", "/config", async (p) => p.getByPlaceholder("Search machines").fill("laptop"), async (p) => (await p.getByPlaceholder("Search machines").inputValue()) === "laptop", undefined, async (p) => clearSearch(p)],
  [
    "Activity: the Streaming view (the filters live in the URL)",
    "/activity",
    async (p) => checked(p, "Streaming").click(),
    async (p) => pressed(p, "Streaming"),
    "team", // the Streaming view is only offered to a team owner/admin/auditor
    async (p) => pressed(p, "Streaming"),
  ],
  [
    "Command palette: open with its text",
    "/tunnels",
    async (p) => {
      if (!(await p.getByPlaceholder("Search pages and machines").count())) await p.keyboard.press("Control+k")
      if (await p.getByPlaceholder("Search pages and machines").count()) await p.getByPlaceholder("Search pages and machines").fill("netw")
    },
    async (p) => (await p.getByPlaceholder("Search pages and machines").count()) > 0 && (await p.getByPlaceholder("Search pages and machines").inputValue()) === "netw",
    undefined,
    async (p) => (await p.getByPlaceholder("Search pages and machines").count()) > 0 && (await p.getByPlaceholder("Search pages and machines").inputValue()) === "netw",
  ],
  [
    "Machine page: the Ended view and search text on its previews tab",
    "/machines/m1/previews",
    async (p) => {
      await checked(p, "Ended").click()
      await p.getByPlaceholder("Search previews").fill("demo")
    },
    async (p) => (await pressed(p, "Ended")) && (await p.getByPlaceholder("Search previews").inputValue()) === "demo",
    undefined,
    async (p) => (await pressed(p, "Ended")) && (await p.getByPlaceholder("Search previews").count()) > 0 && (await p.getByPlaceholder("Search previews").inputValue()) === "demo",
  ],
]

const results = []
let browser
try {
  await appUp()
  browser = await chromium.launch({ executablePath: process.env.PW_CHROMIUM })
  const context = await browser.newContext({ viewport: { width: 1280, height: 1000 } })
  await context.addCookies([{ name: "pb-dev-session", value: "fixture-session", url: ORIGIN }])
  const page = await context.newPage()
  const errors = []
  page.on("pageerror", (error) => errors.push(String(error).slice(0, 80)))
  // React attaches to an element when it hydrates it; typing lands in React only past that.
  // Quiescence (no newly hydrated element between polls) means every boundary is interactive.
  const settled = (timeout) =>
    page.waitForFunction(
      () => {
        const n = [...document.querySelectorAll("button, input, select, textarea, a")].filter((el) =>
          Object.keys(el).some((k) => k.startsWith("__react")),
        ).length
        const last = window.__shellSettledCount ?? -1
        window.__shellSettledCount = n
        return n > 0 && n === last
      },
      null,
      { timeout, polling: 250 },
    )
  // Warm the browser's JS cache once, unheld: a cold profile hydrates slower than the fixture
  // can hold a render, so the first held shell would swap before it is interactive.
  await page.goto(`${ORIGIN}/account`, { waitUntil: "commit" })
  await settled(60000)
  failing = false // reads work from here on. The machine route is never prefetched (no link points at
  // a machine id), so its push always renders the live fallback instead of a cached page.
  // Prime the team workspace the /activity case's Streaming view needs in the cache.
  await page.evaluate(() => window.next.router.push("/team"))
  await settled(60000)
  for (const [name, path, enter, survived, workspace, entered, shell = shellDelay] of cases) {
    if (process.env.ONLY && !name.includes(process.env.ONLY)) continue
    try {
      await context.addCookies([{ name: "pb-workspace", value: workspace ?? "personal", url: ORIGIN }])
      let release
      world.hold = new Promise((resolve) => (release = resolve))
      // A client navigation: the live tree renders the cached static shell at once and stays
      // interactive while the held server render streams in.
      await page.evaluate((to) => window.next.router.push(to), path)
      await shell.wait(page)
      // The shell streams before it hydrates.
      await settled(20000)
      await page.waitForTimeout(400) // the shell shows itself after .shell-delay's 300 ms
      await enter(page)
      // Typing before the shell hydrates is silently lost; once more once it settles.
      if ((await shell.showing(page)) && !(await entered(page).catch(() => false))) {
        await settled(20000)
        await enter(page)
      }
      if (!(await shell.showing(page))) throw new Error("the server page had already replaced the shell: the state was not entered on the shell")
      release()
      await shell.loaded(page)
      await page.waitForTimeout(300)
      if (!(await survived(page))) throw new Error("the state entered on the shell was lost when the server page replaced it")
      results.push([true, name])
    } catch (error) {
      results.push([false, `${name}  (${String(error.message ?? error).split("\n")[0]})`])
    }
  }
  if (errors.length) results.push([false, `page errors: ${errors.join(" | ")}`])
} finally {
  await browser?.close()
  app.kill()
  fixture.close()
}
for (const [ok, name] of results) console.log(`${ok ? "PASS" : "FAIL"}  ${name}`)
const failed = results.filter(([ok]) => !ok).length
console.log(`${results.length - failed}/${results.length} passed`)
process.exit(failed || !results.length ? 1 : 0)
