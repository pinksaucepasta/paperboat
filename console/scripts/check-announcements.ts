// Scripted browser check of the operator announcements in the console: the banner above every page (severity, link,
// dismissal remembered by the server, no layout overflow on a phone, nothing when there is nothing) and the notices in the
// header's bell (unread count, mark read). It runs the BUILT console against a fixture control plane (no real server, no
// database); the fixture keeps what the account did, as the server does.
//   build first:  NEXT_DIST_DIR=.next-check PAPERBOAT_SERVER_URL=http://127.0.0.1:3197 WORKOS_REDIRECT_URI=http://127.0.0.1:3196/auth/callback bunx next build
//   run:          PW_MODULE=<playwright-core index.mjs> PW_CHROMIUM=<chromium> bun scripts/check-announcements.ts [screenshot dir]
// Prints PASS/FAIL per step and exits 1 when one failed.
import { spawn } from "node:child_process"
import { mkdirSync } from "node:fs"
import { sampleFor } from "@/lib/api-sample"

const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const APP = Number(process.env.APP_PORT ?? 3196)
const FIXTURE = Number(process.env.FIXTURE_PORT ?? 3197)
const ORIGIN = `http://127.0.0.1:${APP}`
const shots = process.argv[2] ?? "announcements-shots"
mkdirSync(shots, { recursive: true })

type Banner = { id: string; title: string; body: string; link_url?: string; severity: string; dismissible: boolean }
const state = {
  banner: undefined as Banner | undefined,
  dismissed: new Set<string>(),
  read: new Set<string>(),
  notices: [] as { id: string; title: string; body: string; link_url?: string; sent_at: string }[],
  announcementReads: 0,
  dismissCalls: [] as string[],
}
const ago = (minutes: number) => new Date(Date.now() - minutes * 60_000).toISOString()

const view = () => ({
  banner: state.banner && !state.dismissed.has(state.banner.id) ? state.banner : null,
  notices: state.notices.map((n) => ({ ...n, read: state.read.has(n.id) })),
  unread: state.notices.filter((n) => !state.read.has(n.id)).length,
})

const answers: Record<string, () => unknown> = {
  "/v1/me": () => sampleFor("GET /v1/me", { id: "usr_check", email: "person@example.test", display_name: "Person", created_at: ago(60 * 24 * 30) }),
  "/v1/workspaces": () => ({ items: [{ id: "personal", name: "Personal", kind: "personal", role: "owner" }] }),
  "/v1/billing/workspaces/personal": () =>
    sampleFor("GET /v1/billing/workspaces/personal", { state: "free", allowance_bytes: null, usage_bytes: 0, allowance_period_start: ago(60 * 24 * 3), allowance_period_end: new Date(Date.now() + 27 * 864e5).toISOString(), topups: [], limits: {}, can_manage: true, granted: false }),
  "/v1/billing/workspaces/personal/products": () => ({ items: [] }),
  "/v1/client-configuration": () => ({
    version: "1", cli_verification_url: "https://example.test/cli", machines_url: "https://example.test/machines", abuse_mailbox: "abuse@example.test", issuer: "https://example.test",
    installer_base_url: "https://example.test", console_url: ORIGIN, local_access_base_domain: "local.example.test", preview_base_domain: "preview.example.test",
    tunnel_base_domain: "tunnel.example.test", runtime_base_domain: "runtime.example.test",
  }),
  "/v1/auth/csrf": () => ({ csrf_token: "csrf-check" }),
  "/v1/announcements": view,
  "/v1/team-invitations": () => ({ items: [], pagination: { limit: 1, offset: 0, total: 0, next_offset: null } }),
  "/v1/team-inbox/requests": () => ({ requests: [], pagination: { limit: 1, offset: 0, total: 0, next_offset: null } }),
  "/v1/machines": () => sampleFor("GET /v1/machines", { items: [], pagination: { limit: 50, offset: 0, total: 0, next_offset: null } }),
}

const fixture = Bun.serve({
  port: FIXTURE,
  hostname: "127.0.0.1",
  async fetch(request) {
    const url = new URL(request.url)
    const send = (status: number, body?: unknown) => (body === undefined ? new Response(null, { status }) : Response.json(body, { status }))
    if (request.method === "POST" && url.pathname === "/v1/announcements/read") {
      const { ids } = (await request.json()) as { ids: string[] }
      for (const id of ids) if (state.notices.some((n) => n.id === id)) state.read.add(id)
      state.announcementReads++
      return send(204)
    }
    const dismiss = /^\/v1\/announcements\/([^/]+)\/dismiss$/.exec(url.pathname)
    if (request.method === "POST" && dismiss) {
      if (state.banner?.id !== dismiss[1] || !state.banner.dismissible) return send(404, { error: { code: "not_found", message: "That banner is gone or cannot be dismissed." } })
      state.dismissed.add(dismiss[1])
      state.dismissCalls.push(dismiss[1])
      return send(204)
    }
    const answer = answers[url.pathname]
    if (request.method === "GET" && answer) return send(200, { data: answer() })
    return send(404, { error: { code: "not_found", message: "Not found.", request_id: "req_fixture" } })
  },
})

const app = spawn("bunx", ["next", "start", "-p", String(APP), "-H", "127.0.0.1"], {
  env: { ...process.env, NEXT_DIST_DIR: process.env.NEXT_DIST_DIR ?? ".next-check", PAPERBOAT_SERVER_URL: `http://127.0.0.1:${FIXTURE}`, WORKOS_REDIRECT_URI: `${ORIGIN}/auth/callback` },
  stdio: process.env.DEBUG ? "inherit" : "ignore",
})
for (let i = 0; ; i++) {
  try {
    if ((await fetch(`${ORIGIN}/auth/sign-in`, { redirect: "manual" })).status < 500) break
  } catch {}
  if (i > 80) throw new Error("the console did not start; build it first (see the header)")
  await new Promise((r) => setTimeout(r, 500))
}

const browser = await chromium.launch(process.env.PW_CHROMIUM ? { executablePath: process.env.PW_CHROMIUM } : {})
let failed = 0
const step = async (name: string, fn: () => Promise<void>) => {
  try {
    await fn()
    console.log(`PASS  ${name}`)
  } catch (error) {
    failed++
    console.log(`FAIL  ${name}  (${(error as Error).message.split("\n")[0]})`)
  }
}
const expect = (ok: unknown, message: string) => {
  if (!ok) throw new Error(message)
}

async function open(viewport: { width: number; height: number }, scheme: "light" | "dark" = "light") {
  const context = await browser.newContext({ viewport, colorScheme: scheme })
  await context.addCookies([{ name: "pb-dev-session", value: "check", url: ORIGIN }])
  const page = await context.newPage()
  if (process.env.DEBUG) {
    page.on("pageerror", (e: Error) => console.log("pageerror", e.message.slice(0, 300)))
    page.on("console", (m: { type(): string; text(): string }) => m.type() === "error" && console.log("console.error", m.text().slice(0, 300)))
  }
  await page.goto(ORIGIN, { waitUntil: "networkidle" })
  if (process.env.DEBUG) console.log(page.url(), (await page.locator("body").innerText()).slice(0, 300).replace(/\n/g, " | "))
  return { context, page }
}
// playwright-core is not a dependency of the console (the check loads it from PW_MODULE), so its types are not available.
// eslint-disable-next-line @typescript-eslint/no-explicit-any
const banner = (page: any) => page.locator("[data-announcement-banner]")

state.banner = { id: "ann_beta", title: "Paperboat is in beta.", body: "Things move quickly; tell us what breaks.", link_url: "https://example.test/changelog", severity: "info", dismissible: true }
state.notices = [
  { id: "ann_n1", title: "Relays in Singapore", body: "Machines in Asia now connect through the new relay.", link_url: "/billing", sent_at: ago(20) },
  { id: "ann_n2", title: "Scheduled maintenance", body: "", sent_at: ago(60 * 30) },
]

await step("the banner shows its text, severity and an external link that opens in a new tab", async () => {
  const { context, page } = await open({ width: 1440, height: 900 })
  const b = banner(page)
  await b.waitFor({ timeout: 20000 })
  expect((await b.getAttribute("role")) === "status", "info is a status region")
  expect((await b.innerText()).includes("Paperboat is in beta.") && (await b.innerText()).includes("tell us what breaks"), "text missing")
  const link = b.getByRole("link", { name: "Learn more" })
  expect((await link.getAttribute("target")) === "_blank" && (await link.getAttribute("rel"))?.includes("noopener"), "external link is not new-tab/noopener")
  await page.screenshot({ path: `${shots}/banner-info-desktop-light.png` })
  await context.close()
})

await step("on a phone the banner fits: no sideways scroll, the dismiss target is at least 28 px", async () => {
  const { context, page } = await open({ width: 390, height: 844 })
  await banner(page).waitFor({ timeout: 20000 })
  const wide = await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)
  expect(!wide, "the page scrolls sideways")
  const box = await page.getByRole("button", { name: "Dismiss announcement" }).boundingBox()
  expect(box && box.width >= 28 && box.height >= 28, `dismiss target ${JSON.stringify(box)}`)
  await page.screenshot({ path: `${shots}/banner-info-phone-light.png` })
  await context.close()
})

await step("dark mode and the warning and critical severities", async () => {
  state.banner = { ...state.banner!, severity: "warning", title: "Scheduled maintenance tonight." }
  let opened = await open({ width: 1440, height: 900 }, "dark")
  await banner(opened.page).waitFor({ timeout: 20000 })
  expect((await banner(opened.page).getAttribute("role")) === "status", "warning is a status region")
  await opened.page.screenshot({ path: `${shots}/banner-warning-desktop-dark.png` })
  await opened.context.close()
  state.banner = { ...state.banner!, severity: "critical", title: "Sign-in is degraded." }
  opened = await open({ width: 390, height: 844 }, "dark")
  await banner(opened.page).waitFor({ timeout: 20000 })
  expect((await banner(opened.page).getAttribute("role")) === "alert", "critical is an alert")
  await opened.page.screenshot({ path: `${shots}/banner-critical-phone-dark.png` })
  await opened.context.close()
})

await step("a banner that cannot be dismissed has no dismiss button", async () => {
  state.banner = { ...state.banner!, severity: "info", dismissible: false }
  const { context, page } = await open({ width: 1440, height: 900 })
  await banner(page).waitFor({ timeout: 20000 })
  expect((await page.getByRole("button", { name: "Dismiss announcement" }).count()) === 0, "a dismiss button is shown")
  await context.close()
})

await step("dismissing hides the banner at once and the server remembers it across a reload and a new browser", async () => {
  state.banner = { id: "ann_beta2", title: "New: previews are faster.", body: "", link_url: "/billing", severity: "info", dismissible: true }
  const { context, page } = await open({ width: 1440, height: 900 })
  await banner(page).waitFor({ timeout: 20000 })
  await page.getByRole("button", { name: "Dismiss announcement" }).click()
  await page.waitForFunction(() => !document.querySelector("[data-announcement-banner]"))
  expect(state.dismissCalls.at(-1) === "ann_beta2", `dismiss call ${state.dismissCalls}`)
  await page.reload({ waitUntil: "networkidle" })
  expect((await banner(page).count()) === 0, "back after a reload")
  await context.close()
  const fresh = await open({ width: 1440, height: 900 })
  expect((await banner(fresh.page).count()) === 0, "back in a new browser")
  await fresh.context.close()
})

await step("the bell counts unread notices, lists them with their links, and Mark all as read clears the count for good", async () => {
  const { context, page } = await open({ width: 1440, height: 900 })
  const bell = page.getByRole("button", { name: /^Requests, 2 waiting$/ })
  await bell.waitFor({ timeout: 20000 }).catch(async (e: Error) => {
    const seen = await page.evaluate(() => fetch("/api/v1/announcements").then((r) => r.text()))
    throw new Error(`bell: ${await page.getByRole("button", { name: /^Requests/ }).first().getAttribute("aria-label")} ${seen.slice(0, 200)} ${e.message.split("\n")[0]}`)
  })
  await bell.click()
  const pop = page.locator("[data-slot=popover-content]")
  await pop.getByText("Relays in Singapore").waitFor()
  expect((await pop.locator("[data-notice-read=false]").count()) === 2, "two unread notices")
  expect((await pop.getByRole("link", { name: "Learn more" }).getAttribute("href")) === "/billing", "a console link stays a console link")
  await page.screenshot({ path: `${shots}/bell-unread-desktop-light.png` })
  await pop.getByRole("button", { name: "Mark all as read" }).click()
  await page.getByRole("button", { name: /^Requests$/ }).waitFor()
  expect((await pop.locator("[data-notice-read=true]").count()) === 2, "both read")
  await page.screenshot({ path: `${shots}/bell-read-desktop-light.png` })
  await page.reload({ waitUntil: "networkidle" })
  expect((await page.getByRole("button", { name: /^Requests$/ }).count()) === 1, "unread came back after a reload")
  await context.close()
})

await step("one notice is marked read from its own button, and on a phone the bell fits the screen", async () => {
  state.read.clear()
  const { context, page } = await open({ width: 390, height: 844 })
  await page.getByRole("button", { name: /^Requests, 2 waiting$/ }).click()
  const pop = page.locator("[data-slot=popover-content]")
  await pop.getByText("Scheduled maintenance").waitFor()
  const box = await pop.boundingBox()
  expect(box && box.x >= 0 && box.x + box.width <= 390, `popover ${JSON.stringify(box)}`)
  await pop.getByRole("button", { name: "Mark as read" }).last().click()
  await page.getByRole("button", { name: /^Requests, 1 waiting$/ }).waitFor()
  expect(state.read.has("ann_n2"), "the server was not told")
  await page.screenshot({ path: `${shots}/bell-phone-light.png` })
  await context.close()
})

await step("nothing to say renders nothing: no banner, no badge, the empty bell", async () => {
  state.banner = undefined
  state.notices = []
  const { context, page } = await open({ width: 1440, height: 900 })
  await page.getByRole("button", { name: /^Requests$/ }).waitFor({ timeout: 20000 })
  expect((await banner(page).count()) === 0, "a banner shows")
  await page.getByRole("button", { name: /^Requests$/ }).click()
  await page.locator("[data-slot=popover-content]").getByText("Nothing waiting").waitFor()
  await context.close()
})

await step("a failed announcements read shows nothing and breaks nothing", async () => {
  answers["/v1/announcements"] = () => {
    throw new Error("down")
  }
  const { context, page } = await open({ width: 1440, height: 900 })
  await page.getByRole("button", { name: /^Requests$/ }).waitFor({ timeout: 20000 })
  expect((await banner(page).count()) === 0, "a banner shows")
  await context.close()
})

await browser.close()
app.kill()
fixture.stop(true)
console.log(failed ? `${failed} step(s) failed` : "all steps passed")
process.exit(failed ? 1 : 0)
