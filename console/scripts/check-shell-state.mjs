// Headless check that what the user enters on a page's static shell survives the server page that replaces it.
// On a client navigation Next shows the page's static shell (WithSession's Suspense fallback) while the server renders;
// React then replaces the shell's tree (it never reconciles a fallback with its content), so plain `useState` would be
// lost (lib/use-shell-state.tsx keeps it). For each case: load a page, prefetch the target, hold the server's render
// (the fixture's /v1/me answer waits on a promise: no sleeping), navigate on the client, enter state on the SHELL,
// release, and assert the server page has the same state.
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

// ---- fixture control plane: a signed-in owner of a personal workspace; every list is empty -----------
const world = { hold: null } // a promise the server's per-request session read waits for
const empty = { items: [], pagination: { limit: 10, offset: 0, total: 0, next_offset: null } }
function route(url) {
  const p = url.pathname
  if (p === "/v1/me") return { id: "u", email: "owner@example.test", display_name: "Owner", status: "active", role: "user", workos_subject: "wo_u" }
  if (p === "/v1/workspaces") return { items: [{ id: "personal", name: "Personal", kind: "personal", role: "owner" }] }
  if (p === "/v1/client-configuration") return { purchase_consent: { version: "2026-11-19", text: "Start the service now." } }
  return empty
}
const fixture = http.createServer(async (req, res) => {
  const url = new URL(req.url, `http://127.0.0.1:${FIXTURE_PORT}`)
  if (url.pathname === "/v1/me") await world.hold
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

// ---- the cases: [name, path, what to enter on the shell, what the server page must still show] ---------
const checked = (page, name) => page.getByRole("radio", { name }).first()
const cases = [
  ["Machines: search text", "/", async (p) => p.getByPlaceholder("Search machines").fill("web-1"), async (p) => (await p.getByPlaceholder("Search machines").inputValue()) === "web-1"],
  [
    "Tunnels: search text and the New tunnel dialog with its name",
    "/tunnels",
    async (p) => {
      await p.getByPlaceholder("Search tunnels").fill("api")
      await p.getByRole("button", { name: "New tunnel" }).first().click()
      await p.locator("#tunnel-name").fill("my-tunnel")
    },
    async (p) => (await p.getByPlaceholder("Search tunnels").inputValue()) === "api" && (await p.locator("#tunnel-name").inputValue()) === "my-tunnel",
  ],
  [
    "Previews: the Ended view and search text",
    "/previews",
    async (p) => {
      await checked(p, "Ended").click()
      await p.getByPlaceholder("Search previews").fill("demo")
    },
    async (p) => (await checked(p, "Ended").getAttribute("aria-checked")) === "true" && (await p.getByPlaceholder("Search previews").inputValue()) === "demo",
  ],
  [
    "Terminals: the Closed view and search text",
    "/terminals",
    async (p) => {
      await checked(p, "Closed").click()
      await p.getByPlaceholder("Search terminals").fill("build")
    },
    async (p) => (await checked(p, "Closed").getAttribute("aria-checked")) === "true" && (await p.getByPlaceholder("Search terminals").inputValue()) === "build",
  ],
  ["Network: search text", "/network", async (p) => p.getByPlaceholder("Search servers").fill("relay"), async (p) => (await p.getByPlaceholder("Search servers").inputValue()) === "relay"],
  ["Config sync: search text", "/config", async (p) => p.getByPlaceholder("Search machines").fill("laptop"), async (p) => (await p.getByPlaceholder("Search machines").inputValue()) === "laptop"],
  [
    "Activity: the Streaming view (the filters live in the URL)",
    "/activity",
    async (p) => checked(p, "Streaming").click(),
    async (p) => (await checked(p, "Streaming").getAttribute("aria-checked")) === "true",
  ],
  [
    "Usage: the bandwidth period",
    "/usage",
    async (p) => checked(p, "24 hours").click(),
    async (p) => (await checked(p, "24 hours").getAttribute("aria-checked")) === "true",
  ],
  [
    "New recipe: the name being typed",
    "/settings/recipes/new",
    async (p) => p.locator("#recipe-name").fill("deploy"),
    async (p) => (await p.locator("#recipe-name").inputValue()) === "deploy",
  ],
  [
    "New API token: the name being typed",
    "/settings/api-tokens/new",
    async (p) => p.getByPlaceholder("ci-runner").fill("ci-bot"),
    async (p) => (await p.getByPlaceholder("ci-runner").inputValue()) === "ci-bot",
  ],
  [
    "Command palette: open with its text",
    "/billing",
    async (p) => {
      await p.keyboard.press("Control+k")
      await p.getByRole("combobox").first().fill("netw")
    },
    async (p) => (await p.getByRole("combobox").first().inputValue()) === "netw",
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
  for (const [name, path, enter, survived] of cases) {
    if (process.env.ONLY && !name.includes(process.env.ONLY)) continue
    try {
      world.hold = null
      await page.goto(`${ORIGIN}/account`)
      await page.waitForLoadState("networkidle").catch(() => {})
      // The static shell of the target is in the router cache once its prefetch has answered.
      const prefetched = page.waitForResponse((r) => r.url().includes(`${path === "/" ? "/" : path}?_rsc=`) || (path === "/" && /\/\?_rsc=/.test(r.url())), { timeout: 30000 })
      await page.evaluate((to) => window.next.router.prefetch(to), path)
      await prefetched
      let release
      world.hold = new Promise((resolve) => (release = resolve))
      await page.evaluate((to) => window.next.router.push(to), path)
      await page.waitForSelector(".shell-delay", { state: "attached", timeout: 15000 })
      await page.waitForTimeout(400) // the shell shows itself after .shell-delay's 300 ms
      await enter(page)
      if (!(await page.locator(".shell-delay").count())) throw new Error("the server page had already replaced the shell: the state was not entered on the shell")
      release()
      await page.waitForFunction(() => !document.querySelector(".shell-delay"), null, { timeout: 30000 })
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
