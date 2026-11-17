// Scripted browser check of the admin panel against a fixture control plane (no real server, no database).
// It starts the fixture on FIXTURE_PORT, runs the BUILT app (`next start`, so run `bun run build` first) on APP_PORT,
// drives headless Chromium through sign-in, every page, pagination, filters, error and empty states and a
// support-role session, writes screenshots (light/dark, desktop/phone) and prints PASS/FAIL per step.
//
// usage: PW_MODULE=<playwright-core index.mjs> PW_CHROMIUM=<chromium path> node scripts/check-admin.mjs [screenshot dir]
import { spawn } from "node:child_process"
import { createHmac } from "node:crypto"
import { mkdirSync } from "node:fs"
import http from "node:http"

const { chromium } = await import(process.env.PW_MODULE ?? "playwright-core")
const APP_PORT = Number(process.env.APP_PORT ?? 3199)
const FIXTURE_PORT = Number(process.env.FIXTURE_PORT ?? 3198)
const ORIGIN = `http://127.0.0.1:${APP_PORT}`
const shots = process.argv[2] ?? "admin-check-shots"
mkdirSync(shots, { recursive: true })

// ---- fixture control plane ----------------------------------------------------------------------
// RFC 6238 in a few lines: the check plays the authenticator app.
const b32 = (text) => {
  let bits = ""
  for (const ch of text.replace(/=+$/, "").toUpperCase()) bits += "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567".indexOf(ch).toString(2).padStart(5, "0")
  return Buffer.from(bits.match(/.{8}/g).map((byte) => parseInt(byte, 2)))
}
const totp = (secret, offsetSteps = 0) => {
  const counter = Buffer.alloc(8)
  counter.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 30000) + offsetSteps))
  const mac = createHmac("sha1", b32(secret)).update(counter).digest()
  const at = mac[mac.length - 1] & 15
  return String((mac.readUInt32BE(at) & 0x7fffffff) % 1000000).padStart(6, "0")
}
const mfa = { "tok-admin": { enrolled: false, secret: "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP" }, "tok-support": { enrolled: true, secret: "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" } }
const sessions = { "tok-admin": { email: "root@example.test", role: "admin", csrf: "csrf-admin" }, "tok-support": { email: "agent@example.test", role: "support", csrf: "csrf-support" } }
const seen = { logoutCsrf: undefined, origins: new Set(), cookiesToPaths: [] }
const accounts = Array.from({ length: 120 }, (_, i) => ({
  id: `usr_${String(i).padStart(3, "0")}`,
  primary_email: `person${i}@example.test`,
  display_name: `Person ${i}`,
  status: i % 17 === 0 ? "suspended" : "active",
  version: 1,
  billing_workspace_id: `bw_${i}`,
  created_at: "2026-11-10T08:00:00Z",
}))
const machine = (i, owner = "usr_001") => ({
  id: `host-${i}`, alias: `box-${i}`, account_id: owner, platform: i % 2 ? "linux" : "darwin", architecture: "arm64", state: i % 3 ? "online" : "offline",
  ready: i % 3 !== 0, stale: i % 3 === 0, last_seen_at: "2026-11-17T09:00:00Z", created_at: "2026-11-01T00:00:00Z", connector_state: "connected",
  update_health: "ok", observed_capabilities: ["terminal_host"], installation_generation: 1,
})
const ref = "support_11111111-2222-4333-8444-555555555555"
const page = (items, key, total, url) => {
  const after = url.searchParams.get("after") ?? ""
  const limit = Number(url.searchParams.get("limit") ?? 50)
  const start = after ? items.findIndex((x) => x.id === after) + 1 : 0
  const slice = items.slice(start, start + limit)
  return { [key]: slice, total: total ?? items.length, ...(start + limit < items.length ? { next_after: slice.at(-1)?.id } : {}) }
}
function route(req, url) {
  const p = url.pathname
  if (p === "/v1/admin/accounts") {
    const q = (url.searchParams.get("q") ?? "").toLowerCase()
    const status = url.searchParams.get("status")
    const rows = accounts.filter((a) => (!q || a.primary_email.includes(q) || a.id.includes(q)) && (!status || a.status === status))
    return page(rows, "accounts", rows.length, url)
  }
  if (p.startsWith("/v1/admin/accounts/")) return accounts.find((a) => a.id === decodeURIComponent(p.split("/").pop())) ?? 404
  if (p === "/v1/admin/teams") return { teams: [{ id: "team_a", owner_account: "usr_001", generation: 2, suspended: false, billing_workspace_id: "bw_t", billing_state: "active" }], total: 1 }
  if (p === "/v1/admin/teams/team_a") return { id: "team_a", owner_account: "usr_001", generation: 2, suspended: false, members: [{ account_id: "usr_001", role: "admin", active: true, generation: 1 }], members_truncated: false, billing_workspace_id: "bw_t", billing_state: "active" }
  if (p === "/v1/admin/machines") {
    const owner = url.searchParams.get("account")
    const q = url.searchParams.get("q") ?? ""
    const all = Array.from({ length: 7 }, (_, i) => machine(i)).filter((m) => (!owner || m.account_id === owner) && (!q || m.alias.includes(q) || m.id.includes(q)))
    if (owner === "usr_empty") return { machines: [], total: 0 }
    return page(all, "machines", all.length, url)
  }
  if (p.startsWith("/v1/admin/machines/")) return machine(Number(p.split("-").pop()))
  if (p === "/v1/admin/audit") {
    const rows = Array.from({ length: 3 }, (_, i) => ({ id: `aud_${i}`, actor_user_id: "operator_1", actor_type: "admin", event_type: "operator.request", resource_type: "operator_target", resource_id: "usr_001", outcome: "unchanged", request_id: `req_${i}`, created_at: "2026-11-17T09:00:00Z" }))
    return { events: url.searchParams.get("event_type") === "nothing" ? [] : rows, total: url.searchParams.get("event_type") === "nothing" ? 0 : 3 }
  }
  if (p === "/v1/admin/overview")
    return { accounts: [{ state: "active", count: 110 }, { state: "suspended", count: 10 }], billing_workspaces: [{ state: "active", count: 4 }], control_operations: [{ state: "succeeded", count: 9 }], hosts: 7, hosts_ready: 5, hosts_stale: 2, relays: 2, relays_ready: 2, relays_stale: 0 }
  if (p === "/v1/admin/status") return { observed_at: "2026-11-17T09:00:00Z", build: { version: "dev-abc1234", commit: "abc1234def5678abc1234def5678abc1234def56" }, readiness: "ready", dimensions: { database: { status: "healthy" } } }
  if (p === "/v1/admin/analytics/overview") {
    if (process.env.__FIXTURE_ANALYTICS === "off") return { __status: 503, code: "analytics_not_configured", message: "Product analytics is not configured on this server." }
    return {
      observed_at: "2026-11-17T09:00:00Z", days: Number(url.searchParams.get("days") ?? 30),
      totals: { visitors: 120, page_views: 400, console_users: 30, signup_clicks: 20, add_machine_started: 15, machines_paired: 12, checkouts: 2 },
      daily: [{ day: "2026-11-16", page_views: 100, visitors: 30 }, { day: "2026-11-17", page_views: 300, visitors: 90 }],
      top_pages: [{ name: "/pricing", count: 90 }], referrers: [{ name: "(none)", count: 60 }], countries: [], devices: [{ name: "Desktop", count: 100 }],
      funnel: [{ step: "Site visits", count: 120 }, { step: "Sign-up clicks", count: 20 }, { step: "Add-machine started", count: 15 }, { step: "Machine paired", count: 12 }],
      retention: Number(url.searchParams.get("days")) === 7 ? [{ week: "2026-11-09", size: 5, active: [] }] : [{ week: "2026-11-09", size: 5, active: [4, 3] }, { week: "2026-11-16", size: 2, active: [2] }],
      errors: { countries: "provider_unavailable" },
    }
  }
  if (p === "/v1/admin/sentry/issues") return { issues: [{ project: "go", id: "1", short_id: "GO-1", level: "error", status: "unresolved", count: 5, first_seen: "2026-11-16T00:00:00Z", last_seen: "2026-11-17T08:00:00Z" }] }
  if (p === "/v1/admin/nodes") return { items: [{ id: "node_1", protocol_version: "1.0", region_id: "eu", edge_pool: "a", failure_domain: "x", state: "ready", endpoint_host: "edge.example.test", endpoint_tcp_port: 443, endpoint_quic_port: 443, roles: ["relay"], transports: ["tcp"], capacity_limit: 10, revision: 1, node_generation: 1, registry_expires_at: "2026-11-18T00:00:00Z", ready: true, observed_at: "2026-11-17T09:00:00Z" }], total: 1 }
  if (p === "/v1/admin/regions") return { __status: 503, code: "infrastructure_unavailable", message: "Infrastructure is unavailable.", reference: "support_fixture_regions" }
  if (p === "/v1/admin/infrastructure") return { items: [{ kind: url.searchParams.get("kind"), id: "relay_1", state: "ready", desired_state: "", observed_status: "ready", ready: true, stale: false, revision: 3 }], total: 1 }
  if (p === "/v1/admin/infrastructure/capacity")
    return {
      observed_at: "2026-11-18T09:00:00Z", today: "2026-11-18", month_start: "2026-11-01", total: 4,
      thresholds: { monthly_traffic_bytes: 20e12, traffic_warn_fraction: 0.8, edge_cpu_warn_percent: 50, relay_rss_warn_bytes: 1 << 30, cpu_sustained_seconds: 300 },
      items: [
        { id: "edge-hel-1", kind: "edge", state: "ready", region: "hel", site: "Helsinki", version: "dev-abc1234", cpu_percent: 80, cpu_sustained_percent: 65, rss_bytes: 300e6, connections: 120, connection_limit: 5000, bytes_today: 3e12, bytes_month: 17e12, allowance_used: 0.85, last_heartbeat_at: "2026-11-18T08:59:55Z", metrics_at: "2026-11-18T08:59:55Z",
          warnings: [{ code: "edge_cpu_high", severity: "warning", message: "Sustained CPU is 65% (warning at 50%)." }, { code: "traffic_allowance_high", severity: "warning", message: "This month's traffic 17.0 TB is 85% of the 20.0 TB allowance." }] },
        { id: "relay-fsn-1", kind: "relay", state: "ready", region: "fsn", version: "dev-abc1234", cpu_percent: 5, cpu_sustained_percent: 5, rss_bytes: 2e9, connections: 40, connection_limit: 4096, bytes_today: 1e9, bytes_month: 5e9, allowance_used: 0.00025, last_heartbeat_at: "2026-11-18T08:59:58Z",
          warnings: [{ code: "relay_memory_high", severity: "warning", message: "Memory is 1.9 GiB (warning at 1.0 GiB)." }] },
        { id: "edge-old", kind: "edge", state: "ready", region: "hel", last_heartbeat_at: "2026-11-18T08:50:00Z", warnings: [{ code: "no_metrics", severity: "info", message: "This node does not report CPU, memory or traffic yet (an older build)." }] },
        { id: "server-main", kind: "server", version: "dev-abc1234", cpu_percent: 3, cpu_sustained_percent: 3, rss_bytes: 500e6, last_heartbeat_at: "2026-11-18T08:59:40Z", metrics_at: "2026-11-18T08:59:40Z", warnings: [] },
      ],
    }
  if (p === "/v1/admin/billing/workspaces") return { workspaces: [{ id: "bw_t", scope: "team", scope_id: "team_a", product_code: "team_monthly", state: "active", quantity: 3, period_end: "2026-12-01T00:00:00Z", cancel_at_period_end: false, revision: 1, observed_at: "2026-11-17T09:00:00Z" }], total: 1 }
  if (p === "/v1/admin/billing/workspace-operations") return { operations: [], total: 0 }
  if (p === "/v1/admin/billing/products") return { products: [{ code: "team_monthly", display_name: "Team", catalog_type: "subscription", price_id: "price_1", product_id: "prod_1", unit_amount: 1500, currency: "usd", interval: "month", allowance_bytes: null, active: true, version: 1, price_verified: true }] }
  if (p === `/v1/admin/support/${ref}`)
    return { support_reference: ref, status: "available", records_by_owner: { usr_001: [{ id: "rec_1", owner_id: "usr_001", component: "cli", operation: "install", release: "dev-1", stage: "download", code: "network", cause: "dns_failure", error_type: "net.DNSError", outcome: "failed", severity: "error", at: "2026-11-17T08:00:00Z", expires_at: "2026-11-24T08:00:00Z", reporting: "unconfirmed", support_reference: ref, group_first_seen: "2026-11-15T08:00:00Z", group_last_seen: "2026-11-17T08:00:00Z", group_occurrences: 14 }] }, truncated: false, sentry_links: { evt1: "https://sentry.example.test/e/evt1" }, sentry_search_url: "https://sentry.example.test/s", dropped_records: 0, message: "Evidence available." }
  return 404
}
const fixture = http.createServer((req, res) => {
  const url = new URL(req.url, `http://127.0.0.1:${FIXTURE_PORT}`)
  const send = (status, body, headers = {}) => {
    res.writeHead(status, { "content-type": "application/json", ...headers })
    res.end(JSON.stringify(body))
  }
  const cookie = /pb-operator-session=([^;]+)/.exec(req.headers.cookie ?? "")?.[1]
  const session = cookie ? sessions[cookie] : undefined
  seen.origins.add(req.headers.origin ?? "")
  let body = ""
  req.on("data", (c) => (body += c))
  req.on("end", () => {
    const json = body ? JSON.parse(body) : {}
    if (url.pathname === "/v1/operator/email") return send(200, { data: { next: "password" } })
    if (url.pathname === "/v1/operator/login") {
      const who = json.email === "agent@example.test" ? "tok-support" : "tok-admin"
      if (json.password !== "correct horse battery") return send(401, { error: { code: "invalid_credentials", message: "Operator sign-in failed.", request_id: "req_login_fail" } }, { "request-id": "req_login_fail" })
      const s = sessions[who]
      s.pending = true
      return send(200, { data: { email: s.email, role: s.role, csrf_token: s.csrf, mfa: mfa[who].enrolled ? "verify" : "enroll" } }, { "set-cookie": `pb-operator-session=${who}; Path=/; HttpOnly; Secure; SameSite=Strict` })
    }
    if (!session) return send(401, { error: { code: "unauthenticated", message: "Sign in required.", request_id: "req_unauth" } })
    const state = !session.pending ? "verified" : mfa[cookie].enrolled ? "verify" : "enroll"
    if (url.pathname === "/v1/operator/me") return send(200, { data: { email: session.email, role: session.role, mfa: state } })
    if (url.pathname.startsWith("/v1/operator/mfa/")) {
      if (req.headers["x-csrf-token"] !== session.csrf) return send(403, { error: { code: "forbidden", message: "Operator session or CSRF token is invalid.", request_id: "req_csrf" } })
      const wrong = () => send(401, { error: { code: "invalid_code", message: "That code was not accepted.", request_id: "req_mfa" } }, { "request-id": "req_mfa" })
      const good = [totp(mfa[cookie].secret, -1), totp(mfa[cookie].secret), totp(mfa[cookie].secret, 1)].includes(json.code)
      const codes = Array.from({ length: 10 }, (_, i) => `abcd-efgh-ijk${i}-lmno`)
      if (url.pathname.endsWith("/enroll")) return mfa[cookie].enrolled ? send(409, { error: { code: "mfa_state", message: "That step does not apply to this session.", request_id: "req_state" } }) : send(200, { data: { secret: mfa[cookie].secret, otpauth_uri: `otpauth://totp/Paperboat:${session.email}?secret=${mfa[cookie].secret}&issuer=Paperboat` } })
      if (url.pathname.endsWith("/enroll/confirm")) {
        if (!good) return wrong()
        mfa[cookie].enrolled = true
        session.pending = false
        return send(200, { data: { mfa: "verified", recovery_codes: codes } })
      }
      if (url.pathname.endsWith("/verify")) {
        if (!good && json.code !== "abcd-efgh-ijk0-lmno") return wrong()
        session.pending = false
        return send(200, { data: { mfa: "verified" } })
      }
      if (url.pathname.endsWith("/recovery-codes")) return good && !session.pending ? send(200, { data: { recovery_codes: codes.map((c) => c.replace("abcd", "wxyz")) } }) : wrong()
    }
    if (url.pathname !== "/v1/operator/logout" && session.pending) return send(403, { error: { code: "forbidden", message: "Operator sign-in is required.", request_id: "req_pending" } })
    if (url.pathname === "/v1/operator/logout") {
      seen.logoutCsrf = req.headers["x-csrf-token"]
      return send(204, {}, { "set-cookie": "pb-operator-session=; Path=/; Max-Age=0" })
    }
    if (session.role === "support" && ["/v1/admin/audit", "/v1/admin/overview"].includes(url.pathname)) return send(403, { error: { code: "forbidden", message: "This staff role cannot read this resource.", request_id: "req_forbidden" } })
    const out = route(req, url)
    if (out === 404) return send(404, { error: { code: "not_found", message: "The requested resource was not found.", request_id: "req_404" } })
    if (out.__status) return send(out.__status, { error: { code: out.code, message: out.message, request_id: "req_x", support_reference: out.reference } }, { "support-reference": out.reference ?? "" })
    send(200, { data: out })
  })
})
await new Promise((resolve) => fixture.listen(FIXTURE_PORT, "127.0.0.1", resolve))

// ---- the built app ---------------------------------------------------------------------------------
const app = spawn("bunx", ["next", "start", "-p", String(APP_PORT), "-H", "127.0.0.1"], {
  env: { ...process.env, ADMIN_ORIGIN: ORIGIN, ADMIN_PAPERBOAT_SERVER_URL: `http://127.0.0.1:${FIXTURE_PORT}`, NEXT_DIST_DIR: process.env.NEXT_DIST_DIR ?? ".next" },
  stdio: "ignore",
})
async function appUp() {
  for (let i = 0; i < 60; i++) {
    try {
      if ((await fetch(`${ORIGIN}/login`)).ok) return
    } catch {}
    await new Promise((r) => setTimeout(r, 500))
  }
  throw new Error("the app did not start; run `bun run build` first")
}

const results = []
const step = async (name, fn) => {
  try {
    await fn()
    results.push([true, name])
  } catch (error) {
    results.push([false, `${name}  (${String(error.message ?? error).split("\n")[0]})`])
  }
}
const expect = (cond, message) => {
  if (!cond) throw new Error(message)
}

let browser
try {
  await appUp()
  browser = await chromium.launch(process.env.PW_CHROMIUM ? { executablePath: process.env.PW_CHROMIUM } : { channel: process.env.PW_CHANNEL })
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 } })
  const p = await context.newPage()
  const text = async (sel) => (await p.locator(sel).first().innerText()).trim()
  const waitReady = (what) => p.waitForSelector(`[data-table="${what}"][data-state="ready"], [data-table="${what}"][data-state="empty"]`, { timeout: 15000 })

  await step("signed-out visitors are sent to sign-in", async () => {
    await p.goto(`${ORIGIN}/users`)
    await p.waitForURL(/\/login/)
    await p.waitForSelector("#email", { timeout: 15000 })
    expect(new URL(p.url()).searchParams.get("return_to") === "/users", "return_to was not kept")
    await p.goto(`${ORIGIN}/login`)
  })
  await step("a customer-style wrong password shows the error with its reference", async () => {
    await p.fill("#email", "root@example.test")
    await p.click('button[type="submit"]')
    await p.fill("#password", "wrong")
    await p.click('button[type="submit"]')
    await p.waitForSelector("[data-login-error]")
    expect((await text("[data-login-error]")).includes("req_login_fail"), "no reference shown")
  })
  const adminSecret = mfa["tok-admin"].secret
  await step("first sign-in asks the operator to enrol an authenticator: key, QR, wrong code refused with its reference", async () => {
    await p.fill("#password", "correct horse battery")
    await p.click('button[type="submit"]')
    await p.waitForSelector("[data-mfa-enroll]")
    expect((await text("[data-mfa-secret]")) === adminSecret, "the key shown is not the server's")
    await p.waitForSelector('img[alt="Authenticator QR code"]')
    // The pending session cannot read admin data.
    const early = await p.evaluate(async () => (await fetch("/api/v1/admin/accounts")).status)
    expect(early === 403, `a pending session read admin data: ${early}`)
    await p.fill("#code", "000000")
    await p.click('button[type="submit"]')
    await p.waitForSelector("[data-login-error]")
    expect((await text("[data-login-error]")).includes("req_mfa"), "no reference on a wrong code")
  })
  await step("a right code shows recovery codes once, then the overview opens; CSRF cookie is HttpOnly", async () => {
    await p.fill("#code", totp(adminSecret))
    await p.click('button[type="submit"]')
    await p.waitForSelector("[data-recovery-codes]")
    expect((await p.locator("[data-recovery-codes] li").count()) === 10, "ten recovery codes expected")
    await p.screenshot({ path: `${shots}/recovery-codes-light.png`, fullPage: true })
    await p.getByRole("button", { name: "I saved them, open the panel" }).click()
    await p.waitForURL(/\/overview/)
    await p.waitForSelector("[data-overview-ready]")
    expect(!(await p.evaluate(() => document.cookie)).includes("pb-admin-csrf"), "csrf cookie readable by scripts")
    const cookies = await context.cookies()
    expect(cookies.some((c) => c.name === "pb-admin-csrf" && c.httpOnly), "no HttpOnly csrf cookie")
  })
  await step("security page replaces recovery codes only with a good authenticator code", async () => {
    await p.goto(`${ORIGIN}/security`)
    await p.fill('[aria-label="Authenticator code"]', "000000")
    await p.getByRole("button", { name: "Replace recovery codes" }).click()
    await p.waitForSelector("[data-security-error]")
    expect((await text("[data-security-error]")).includes("req_mfa"), "no reference")
    await p.fill('[aria-label="Authenticator code"]', totp(adminSecret))
    await p.getByRole("button", { name: "Replace recovery codes" }).click()
    await p.waitForSelector("[data-recovery-codes]")
    expect((await text("[data-recovery-codes]")).includes("wxyz"), "new codes not shown")
  })
  await step("overview shows key numbers, control plane status and PostHog insights", async () => {
    await p.goto(`${ORIGIN}/overview`)
    await p.waitForSelector("[data-overview-ready]")
    expect((await text('[data-stat="Accounts"]')).includes("120"), "accounts total")
    await p.waitForSelector('[data-analytics-state="ready"]')
    expect((await text('[data-analytics-state="ready"]')).includes("/pricing"), "top pages")
    expect((await text('[data-analytics-state="ready"]')).includes("countries (provider_unavailable)"), "partial insight failure is named")
    expect((await text("[data-build]")).includes("dev-abc1234"), "server build version not shown")
    expect((await text('[data-retention-state="ready"]')).includes("80%"), "retention percentage")
    await p.getByRole("tab", { name: "7 days" }).click()
    await p.waitForSelector('[data-retention-state="not-configured"]')
    await p.getByRole("tab", { name: "30 days" }).click()
    await p.waitForSelector('[data-retention-state="ready"]')
    await p.screenshot({ path: `${shots}/overview-light.png`, fullPage: true })
  })
  await step("users: real total, page 1 of 3, Next shows 51-100, Previous comes back", async () => {
    await p.goto(`${ORIGIN}/users`)
    await waitReady("users")
    expect((await text("[data-list-range]")).replace(/\s+/g, " ") === "1–50 of 120", `range was ${await text("[data-list-range]")}`)
    await p.getByRole("button", { name: "Next" }).click()
    await p.waitForFunction(() => document.querySelector("[data-list-range]")?.textContent?.startsWith("51"))
    expect(await text("[data-list-range]") === "51–100 of 120", "second page range")
    await p.getByRole("button", { name: "Next" }).click()
    await p.waitForFunction(() => document.querySelector("[data-list-range]")?.textContent?.startsWith("101"))
    expect(await text("[data-list-range]") === "101–120 of 120", "last page range")
    expect(await p.getByRole("button", { name: "Next" }).isDisabled(), "Next must be disabled on the last page")
    await p.getByRole("button", { name: "Previous" }).click()
    await p.waitForFunction(() => document.querySelector("[data-list-range]")?.textContent?.startsWith("51"))
  })
  await step("users: search narrows the list and the total follows", async () => {
    await p.fill('[aria-label="Search users"]', "person11")
    await p.waitForFunction(() => document.querySelector("[data-list-range]")?.textContent?.includes("of 11") || document.querySelector("[data-list-range]")?.textContent?.includes("11 total"))
    expect((await text("[data-list-range]")).includes("11"), "total did not follow the search")
    await p.screenshot({ path: `${shots}/users-light.png`, fullPage: true })
  })
  await step("users: a search with no match shows the empty state", async () => {
    await p.fill('[aria-label="Search users"]', "nobody-at-all")
    await p.waitForSelector('[data-table="users"][data-state="empty"]')
    expect(await p.getByText("No account matches these filters.").isVisible(), "empty text")
  })
  await step("user detail lists the owner's machines", async () => {
    await p.goto(`${ORIGIN}/users/usr_001`)
    await p.waitForSelector('[data-table="machines"][data-state="ready"]')
    expect(await p.getByText("person1@example.test").first().isVisible(), "email")
  })
  await step("a user that does not exist shows the error with its reference", async () => {
    await p.goto(`${ORIGIN}/users/usr_missing`)
    await p.waitForSelector("[data-error-block]")
    expect((await text("[data-error-block]")).includes("req_404"), "reference missing")
  })
  for (const [path, what] of [["/teams", "teams"], ["/machines", "machines"], ["/audit", "audit events"], ["/sentry", "Sentry issues"]]) {
    await step(`${path} loads rows`, async () => {
      await p.goto(`${ORIGIN}${path}`)
      await waitReady(what)
      expect(await p.locator(`[data-table="${what}"] tbody tr`).count() > 0, "no rows")
    })
  }
  await step("audit: a filter with no events shows the empty state", async () => {
    await p.goto(`${ORIGIN}/audit`)
    await p.fill('[aria-label="Event type"]', "nothing")
    await p.waitForSelector('[data-table="audit events"][data-state="empty"]')
  })
  await step("relays & edges: nodes load; regions failure shows its reference", async () => {
    await p.goto(`${ORIGIN}/infrastructure`)
    await waitReady("nodes")
    await p.getByRole("tab", { name: "Regions" }).click()
    await p.waitForSelector("[data-error-block]")
    expect((await text("[data-error-block]")).includes("support_fixture_regions"), "reference missing")
    await p.getByRole("tab", { name: "Relay health" }).click()
    await waitReady("relay health")
  })
  await step("capacity: every node with CPU, memory, traffic and the three warnings", async () => {
    await p.goto(`${ORIGIN}/capacity`)
    await p.waitForSelector("[data-capacity-ready]")
    expect((await p.locator("[data-table=capacity] tbody tr").count()) === 4, "four nodes expected")
    expect((await text('[data-node="edge-hel-1"] [data-warning="edge_cpu_high"]')).includes("65%"), "edge CPU warning")
    expect((await text('[data-node="edge-hel-1"] [data-warning="traffic_allowance_high"]')).includes("85%"), "traffic warning")
    expect((await text('[data-node="relay-fsn-1"] [data-warning="relay_memory_high"]')).includes("1.9 GiB"), "relay memory warning")
    expect((await p.locator('[data-node="server-main"]').getAttribute("data-level")) === "ok", "a healthy server has no warning")
    expect((await text('[data-node="edge-old"]')).includes("older build"), "node without metrics says so")
    expect((await text('[data-stat="Need attention"]')).includes("2"), "two nodes need attention")
    await p.screenshot({ path: `${shots}/capacity-light.png`, fullPage: true })
  })
  await step("billing: workspaces, operations (empty) and plans", async () => {
    await p.goto(`${ORIGIN}/billing`)
    await waitReady("billing workspaces")
    await p.getByRole("tab", { name: "Operations" }).click()
    await p.waitForSelector('[data-table="billing operations"][data-state="empty"]')
    await p.getByRole("tab", { name: "Plans & prices" }).click()
    await waitReady("plans and prices")
    expect(await p.getByText("15.00 USD / month").isVisible(), "price")
  })
  await step("support lookup joins the stored fault, Sentry link, owner and machines", async () => {
    await p.goto(`${ORIGIN}/support`)
    await p.fill('[aria-label="Support reference"]', "not-a-reference")
    await p.click('button[type="submit"]')
    await p.waitForSelector("[data-lookup-invalid]")
    await p.fill('[aria-label="Support reference"]', ref)
    await p.click('button[type="submit"]')
    await p.waitForSelector("[data-lookup-result]")
    await p.waitForSelector("[data-owner-card] [data-table]")
    const body = await text("[data-lookup-result]")
    expect(body.includes("dns_failure") && body.includes("evt1") && body.includes("person1@example.test"), "joined data missing")
    expect((await text("[data-failure-group]")).includes("14 occurrences"), "failure group statistics not shown")
    await p.screenshot({ path: `${shots}/support-light.png`, fullPage: true })
  })
  await step("the relay serves only operator and admin APIs and refuses cross-origin writes", async () => {
    const closed = await p.evaluate(async () => (await fetch("/api/v1/me")).status)
    expect(closed === 404, `/api/v1/me answered ${closed}`)
    const status = await context.request.post(`${ORIGIN}/api/v1/operator/logout`, { headers: { origin: "https://evil.example.test" } })
    expect(status.status() === 403, `forged-origin logout answered ${status.status()}`)
  })
  await step("dark theme and phone width keep the layout", async () => {
    await p.goto(`${ORIGIN}/users`)
    await waitReady("users")
    await p.emulateMedia({ colorScheme: "dark" })
    await p.screenshot({ path: `${shots}/users-dark.png`, fullPage: true })
    await p.setViewportSize({ width: 390, height: 800 })
    await p.screenshot({ path: `${shots}/users-phone.png`, fullPage: true })
    expect(await p.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth + 1), "page scrolls sideways at 390px")
    await p.setViewportSize({ width: 1280, height: 800 })
  })
  await step("sign-out sends the CSRF token and ends the session", async () => {
    await p.goto(`${ORIGIN}/overview`)
    await p.waitForSelector("[data-overview-ready]")
    await p.getByRole("button", { name: "Sign out" }).click()
    await p.waitForURL(/\/login/)
    expect(seen.logoutCsrf === "csrf-admin", `logout csrf was ${seen.logoutCsrf}`)
    await p.goto(`${ORIGIN}/users`)
    await p.waitForURL(/\/login/)
  })
  await step("an enrolled operator is asked for a code; a reload resumes; a wrong code names its reference; a recovery code works", async () => {
    await p.fill("#email", "agent@example.test")
    await p.click('button[type="submit"]')
    await p.fill("#password", "correct horse battery")
    await p.click('button[type="submit"]')
    await p.waitForSelector('[data-login-step="verify"]')
    await p.reload()
    await p.waitForSelector('[data-login-step="verify"]')
    const early = await p.evaluate(async () => (await fetch("/api/v1/admin/accounts")).status)
    expect(early === 403, `a pending session read admin data: ${early}`)
    await p.fill("#code", "123456")
    await p.click('button[type="submit"]')
    await p.waitForSelector("[data-login-error]")
    expect((await text("[data-login-error]")).includes("req_mfa"), "no reference on a wrong code")
    await p.fill("#code", "abcd-efgh-ijk0-lmno")
    await p.click('button[type="submit"]')
    await p.waitForURL(/\/users/)
  })
  await step("a support-role operator sees only its pages and a refusal says so with a reference", async () => {
    await p.waitForSelector('[data-slot="sidebar-menu"] a')
    const nav = await p.locator('[data-slot="sidebar-menu"] a').allInnerTexts()
    expect(!nav.some((t) => t.includes("Audit log")) && nav.some((t) => t.includes("Users")), `nav was ${nav.join("|")}`)
    await p.goto(`${ORIGIN}/audit`)
    await p.waitForSelector("[data-error-block]")
    expect((await text("[data-error-block]")).includes("req_forbidden"), "403 reference missing")
    await p.screenshot({ path: `${shots}/forbidden-light.png`, fullPage: true })
  })
} catch (error) {
  results.push([false, `check aborted  (${String(error.message ?? error).split("\n")[0]})`])
} finally {
  await browser?.close()
  app.kill()
  fixture.close()
}
let failed = 0
for (const [ok, name] of results) {
  if (!ok) failed++
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}`)
}
console.log(`${results.length - failed}/${results.length} passed`)
process.exit(failed || results.length === 0 ? 1 : 0)
