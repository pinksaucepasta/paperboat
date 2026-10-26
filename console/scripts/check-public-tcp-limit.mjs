// Console check for the public TCP limit: adding a public hostname that the server refuses with 409
// public_tcp_limit_reached shows the server's sentence with Upgrade and "Use your own edge", and the
// billing overview shows "Public TCP tunnels" from limits.public_tcp. Both answers are stubbed in the browser
// (the real refusal needs a Free workspace at its limit); the page and its wiring are the real console.
//   STATE=<storage state> CHROME=<Chromium> CONSOLE_ORIGIN=<console origin> TUNNEL=<tunnel id> node check-public-tcp-limit.mjs
import { chromium } from "playwright-core"

const { STATE, CHROME, CONSOLE_ORIGIN: origin, TUNNEL: tunnel } = process.env
const browser = await chromium.launch({ executablePath: CHROME })
const ctx = await browser.newContext({ storageState: STATE, viewport: { width: 1280, height: 800 } })
let failed = 0
const log = (ok, step, detail = "") => {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${step}${detail ? `  (${detail})` : ""}`)
}

const message =
  "This workspace has reached its plan's limit of 1 public TCP tunnels. Stop one you no longer need, upgrade your plan, or run your own tunnel edge (self-hosted tunnels are not limited). No resource was created."
const p = await ctx.newPage()
await p.route(/\/api\/v1\/tunnels\/[^/]+\/routes(\?.*)?$/, (route) =>
  route.request().method() === "POST"
    ? route.fulfill({
        status: 409,
        contentType: "application/json",
        body: JSON.stringify({ error: { schema: "paperboat.preview-tunnel/v1", kind: "error", code: "public_tcp_limit_reached", component: "control", message, outcome: "unchanged", retryable: false } }),
      })
    : route.continue(),
)
await p.goto(`${origin}/tunnels/${tunnel}`, { waitUntil: "networkidle", timeout: 120000 })
await p.getByRole("button", { name: "Add", exact: true }).first().click()
await p.locator("#hostname-service").fill("3000")
await p.getByRole("button", { name: "Add hostname" }).click()
const alert = p.getByRole("alert").filter({ hasText: "public TCP" })
log(await alert.isVisible({ timeout: 15000 }).catch(() => false), "route add: the server's sentence is shown", "alert")
log(((await alert.innerText().catch(() => "")) || "").includes("run your own tunnel edge"), "...with its self-host option in the words")
log((await alert.getByRole("link", { name: "Upgrade" }).getAttribute("href").catch(() => null)) === "/billing", "...an Upgrade action to Billing")
log((await alert.getByRole("link", { name: "Use your own edge" }).getAttribute("href").catch(() => null)) === "/network", "...and 'Use your own edge'")
await p.screenshot({ path: "/tmp/public-tcp-limit-route.png" })

// Billing overview: the line from the server's numbers (stubbed workspace billing with limits.public_tcp).
const q = await ctx.newPage()
await q.route(/\/api\/v1\/billing\/workspaces\/personal(\?.*)?$/, async (route) => {
  const real = await route.fetch()
  const body = await real.json()
  body.data = { ...body.data, limits: { ...(body.data?.limits ?? { machines: { used: 1, limit: 100 }, publications: { used: 0, limit: 100 }, browser_terminals: { used: 0, limit: 10 } }), public_tcp: { used: 1, limit: 1 } } }
  await route.fulfill({ response: real, json: body })
})
await q.goto(`${origin}/billing`, { waitUntil: "networkidle", timeout: 120000 })
const row = q.getByText("Public TCP tunnels")
log(await row.isVisible({ timeout: 15000 }).catch(() => false), "billing: 'Public TCP tunnels' is listed with the plan limits")
log(((await row.locator("..").innerText().catch(() => "")) || "").includes("1 of 1"), "...as used of limit", "1 of 1")
await q.screenshot({ path: "/tmp/public-tcp-limit-billing.png" })
await browser.close()
console.log(failed ? `${failed} failed` : "all passed")
process.exit(failed ? 1 : 0)
