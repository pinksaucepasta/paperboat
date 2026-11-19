// Scripted browser check of the admin panel against a fixture control plane (no real server, no database).
// It starts the fixture on FIXTURE_PORT, runs the BUILT app (`next start`, so run `bun run build` first) on APP_PORT,
// drives headless Chromium through sign-in, every page, pagination, filters, error and empty states and a
// support-role session, writes screenshots (light/dark, desktop/phone) and prints PASS/FAIL per step.
//
// usage: PW_MODULE=<playwright-core index.mjs> PW_CHROMIUM=<chromium path> node scripts/check-admin.mjs [screenshot dir]
import { spawn } from "node:child_process"
import { createHmac } from "node:crypto"
import { mkdirSync, readFileSync } from "node:fs"
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
const mfa = { "tok-admin": { enrolled: false, secret: "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP" }, "tok-support": { enrolled: true, secret: "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" }, "tok-new": { enrolled: false, secret: "MFRGGZDFMZTWQ2LKNNWG23TPOBYXE43U" } }
const sessions = { "tok-admin": { email: "root@example.test", role: "admin", csrf: "csrf-admin" }, "tok-support": { email: "agent@example.test", role: "support", csrf: "csrf-support" }, "tok-new": { email: "new@example.test", role: "billing", csrf: "csrf-new" } }
const seen = { setup: undefined, logoutCsrf: undefined, origins: new Set(), cookiesToPaths: [] }
const accounts = Array.from({ length: 120 }, (_, i) => ({
  id: `usr_${String(i).padStart(3, "0")}`,
  primary_email: `person${i}@example.test`,
  display_name: `Person ${i}`,
  status: i % 17 === 0 ? "suspended" : "active",
  ...(i % 17 === 0 ? { ban_reason: "ABUSE-9", banned_at: "2026-11-12T08:00:00Z" } : {}),
  version: 1,
  billing_workspace_id: `bw_${i}`,
  created_at: "2026-11-10T08:00:00Z",
}))
const machine = (i, owner = "usr_001") => ({
  id: `host-${i}`, alias: `box-${i}`, account_id: owner, platform: i % 2 ? "linux" : "darwin", architecture: "arm64", state: i % 3 ? "online" : "offline",
  ready: i % 3 !== 0, stale: i % 3 === 0, last_seen_at: "2026-11-17T09:00:00Z", created_at: "2026-11-01T00:00:00Z", connector_state: "connected",
  update_health: "ok", observed_capabilities: ["terminal_host"], installation_generation: 1,
})
// The server's own write policy (capability, where the Operator-Target comes from), read from its source so the
// fixture refuses exactly what the real guard refuses: a wrong target, an unknown route or a role without authority.
const policies = [...readFileSync(new URL("../../server/internal/httpapi/operator_handlers.go", import.meta.url), "utf8").matchAll(/^\s*"(POST|PUT|DELETE) (\/v1\/[^"]+)":\s*\{"(\w+)", "([^"]*)", "([^"]*)"\}/gm)].map((m) => ({
  method: m[1], regex: new RegExp(`^${m[2].replace(/\{(\w+)\}/g, "(?<$1>[^/]+)")}$`), capability: m[3], parameter: m[4], target: m[5],
}))
if (policies.length < 40) throw new Error(`parsed only ${policies.length} write policies from the server source`)
const allowedRole = (role, capability) => role === "admin" || (capability === "support" && ["support", "billing"].includes(role)) || (capability === "billing" && role === "billing") || (capability === "operations" && role === "operations")
function expectedTarget(method, path, body) {
  for (const policy of policies) {
    const m = policy.method === method && policy.regex.exec(path)
    if (!m) continue
    const target = policy.parameter === "" ? policy.target : policy.parameter.startsWith("body:") ? body[policy.parameter.slice(5)] : m.groups[policy.parameter]
    return { capability: policy.capability, target: decodeURIComponent(String(target ?? "")) }
  }
}
// A live operator session: the server lists only sessions that have not expired, so the fixture's are relative to now.
const fixtureSession = (c) => ({ id: c.repeat(64), created_at: new Date(Date.now() - 3600e3).toISOString(), expires_at: new Date(Date.now() + 7 * 3600e3).toISOString(), mfa_pending: false })
// What the panel changed, and the state the fixture serves back (so a write is visible on the next read).
const writes = []
const world = {
  regionsFail: true,
  regions: [{ id: "eu", display_name: "Europe", enabled: true, revision: 2, updated_at: "2026-11-17T09:00:00Z" }],
  nodes: [{ id: "node_1", protocol_version: "1.0", region_id: "eu", edge_pool: "a", failure_domain: "x", state: "ready", endpoint_host: "edge.example.test", endpoint_tcp_port: 443, endpoint_quic_port: 443, roles: ["relay"], transports: ["tcp"], capacity_limit: 10, revision: 1, node_generation: 1, registry_expires_at: "2026-11-18T00:00:00Z", ready: true, observed_at: "2026-11-17T09:00:00Z" },
    { id: "edge_1", protocol_version: "1.0", region_id: "eu", edge_pool: "a", failure_domain: "x", state: "ready", endpoint_host: "edge-1.example.test", endpoint_tcp_port: 443, endpoint_quic_port: 8443, roles: ["edge"], transports: ["http3", "http2"], capacity_limit: 10, revision: 4, node_generation: 1, registry_expires_at: "2026-11-18T00:00:00Z", ready: true, observed_at: "2026-11-17T09:00:00Z" }],
  installs: [],
  serverNodes: [{ id: "srv_1", region: "eu", state: "active", version: "dev-abc1234", commit: "abc1234", started_at: "2026-11-17T09:00:00Z", last_seen_at: "2026-11-17T09:00:00Z", healthy: true, public_address: "203.0.113.9" }],
  policy: { personal_allowance_bytes: 5e9, team_allowance_bytes: null, revision: 3 },
  operators: [
    { id: "operator_root", email: "root@example.test", role: "admin", status: "active", mfa_enrolled: true, invited: false, live_sessions: 1, created_at: "2026-11-01T08:00:00Z" },
    { id: "operator_agent", email: "agent@example.test", role: "support", status: "active", mfa_enrolled: true, invited: false, live_sessions: 2, created_at: "2026-11-05T08:00:00Z" },
  ],
  operatorSessions: { operator_agent: ["a", "b"].map((c) => fixtureSession(c)) },
  grants: {},
  entitlementGrants: {},
  teamBan: undefined,
  ipBlocks: [
    { id: "ipb_1", cidr: "198.51.100.0/24", reason: "ABUSE-3", created_by: "operator_root", created_at: "2026-11-15T08:00:00Z", running: true },
    { id: "ipb_0", cidr: "192.0.2.7/32", reason: "ABUSE-1", created_by: "operator_root", created_at: "2026-11-10T08:00:00Z", expires_at: "2026-11-11T08:00:00Z", running: false },
  ],
  announcements: [
    { id: "ann_2", kind: "banner", title: "Paperboat is in beta", body: "Tell us what breaks.", severity: "info", dismissible: true, audience_kind: "all", starts_at: "2026-11-16T08:00:00Z", created_by: "operator_root", created_at: "2026-11-16T08:00:00Z", updated_at: "2026-11-16T08:00:00Z", state: "live", reads: 0, dismissals: 12 },
    { id: "ann_1", kind: "notice", title: "Relays in Singapore", body: "", audience_kind: "plan", audience_value: "pro_monthly", starts_at: "2026-11-10T08:00:00Z", ends_at: "2026-11-11T08:00:00Z", created_by: "operator_root", created_at: "2026-11-10T08:00:00Z", updated_at: "2026-11-10T08:00:00Z", state: "ended", severity: "info", dismissible: true, reads: 31, dismissals: 0 },
  ],
  notices: [],
  auditExtra: [],
  floor: { schema: "paperboat.client-update-policy/v1", revision: 2, minimum_version: "", reason: "" },
  requests: [],
}
const ref = "support_11111111-2222-4333-8444-555555555555"
// 120 offline machines, 24 in each "unseen for" bucket (the last bucket never reported), at the fixture's now of 2026-11-18T09:00Z.
const fleetSeen = { under_1_day: "2026-11-18T06:00:00Z", "1_to_7_days": "2026-11-14T06:00:00Z", "7_to_30_days": "2026-11-01T06:00:00Z", over_30_days: "2026-09-20T06:00:00Z" }
const fleetOffline = Array.from({ length: 120 }, (_, i) => {
  const bucket = ["under_1_day", "1_to_7_days", "7_to_30_days", "over_30_days", "never_seen"][Math.floor(i / 24)]
  return { id: `host-off-${String(i).padStart(3, "0")}`, alias: `box-${i}`, platform: ["linux", "darwin", "windows"][i % 3], architecture: i % 3 === 1 ? "arm64" : "amd64", version: i % 5 === 0 ? "dev-abc1234" : i % 2 ? "2026.11.5.1" : "2026.10.28.2", state: "offline", account_id: `usr_${String(i % 40).padStart(3, "0")}`, bucket, ...(fleetSeen[bucket] ? { last_seen_at: fleetSeen[bucket] } : {}) }
})
const page = (items, key, total, url) => {
  const after = url.searchParams.get("after") ?? ""
  const limit = Number(url.searchParams.get("limit") ?? 50)
  const start = after ? items.findIndex((x) => x.id === after) + 1 : 0
  const slice = items.slice(start, start + limit)
  return { [key]: slice, total: total ?? items.length, ...(start + limit < items.length ? { next_after: slice.at(-1)?.id } : {}) }
}
function mutate(method, p, json) {
  const [, , , area, id, action] = p.split("/").map(decodeURIComponent)
  const entitlementRoute = /^\/v1\/admin\/billing\/workspaces\/([^/]+)\/entitlements(\/revoke)?$/.exec(p)
  if (entitlementRoute) {
    const workspace = decodeURIComponent(entitlementRoute[1])
    const grants = (world.entitlementGrants[workspace] ??= [])
    if (entitlementRoute[2]) {
      const grant = grants.find((g) => g.id === json.grant_id && g.running)
      if (!grant) return { __status: 404, code: "not_found", message: "The target was not found.", reference: "support_fixture_grant" }
      Object.assign(grant, { running: false, revoked_at: new Date().toISOString() })
      return { resource_id: workspace, state: "revoked", version: 0, grant_id: grant.id }
    }
    if (json.entitlement !== "bandwidth") for (const g of grants) if (g.kind === json.entitlement && g.running) Object.assign(g, { running: false, revoked_at: new Date().toISOString() })
    const grant = { id: `grant_${grants.length + 1}`, workspace_id: workspace, kind: json.entitlement, amount: json.amount, ends_at: json.ends_at, reason: "x", created_by: "operator_root", created_at: new Date().toISOString(), running: true }
    grants.push(grant)
    return { resource_id: workspace, state: "granted", version: 0, grant_id: grant.id }
  }
  if (area === "accounts" && (action === "ban" || action === "unban")) {
    const account = accounts.find((a) => a.id === id)
    for (const ip of action === "ban" ? (json.block_ips ?? []) : []) world.ipBlocks.unshift({ id: `ipb_${world.ipBlocks.length + 5}`, cidr: ip.includes("/") ? ip : `${ip}/32`, reason: writes.at(-1).reason, created_by: "operator_root", created_at: new Date().toISOString(), running: true })
    if (action === "ban") Object.assign(account, { status: "suspended", ban_reason: writes.at(-1).reason, banned_at: new Date().toISOString(), version: account.version + 1 })
    else Object.assign(account, { status: "active", ban_reason: undefined, banned_at: undefined, version: account.version + 1 })
    return { resource_id: id, state: action === "ban" ? "suspended" : "active", version: account.version, teams: action === "ban" ? 1 : 0 }
  }
  if (area === "teams" && (action === "ban" || action === "unban")) {
    world.teamBan = action === "ban" ? { reason: writes.at(-1).reason, at: new Date().toISOString() } : undefined
    return { resource_id: id, state: action === "ban" ? "suspended" : "active", version: json.revision + 1 }
  }
  if (area === "notices") {
    // The server refuses a price increase under 30 days away and renders the mail; a dry run sends nothing.
    if (json.kind === "price_increase" && !(Date.parse(json.effective) >= Date.now() + 29 * 86400e3)) return { __status: 400, code: "invalid_request", message: "The effective date must be at least 30 days away.", reference: "support_fixture_notice" }
    const notice = { kind: json.kind, audience_kind: json.audience_kind, audience_value: json.audience_value || undefined, subject: `Paperboat: prices change on ${json.effective}`, html: `<p>${json.detail}</p>`, recipients: json.audience_kind === "user" ? 1 : 2 }
    if (json.dry_run) return notice
    const sent = { id: `notice_${world.notices.length + 1}`, ...notice, created_by: "operator_root", created_at: new Date().toISOString(), queued_at: new Date().toISOString() }
    world.notices.unshift(sent)
    world.auditExtra.unshift({ id: `aud_notice_${world.notices.length}`, actor_user_id: "operator_root", actor_type: "admin", event_type: "operator.customer_notice_send", resource_type: "customer_notice", resource_id: sent.id, outcome: "success", request_id: "req_notice", created_at: sent.created_at })
    return sent
  }
  if (area === "announcements") {
    const now = new Date().toISOString()
    const live = world.announcements.find((a) => a.id === id)
    if (id && !live) return { __status: 404, code: "not_found", message: "The announcement was not found.", reference: "support_fixture_ann" }
    if (action === "retract") return Object.assign(live, { state: "retracted", retracted_at: now })
    const clash = json.kind === "banner" && world.announcements.find((a) => a.id !== id && a.kind === "banner" && a.state === "live" && a.audience_kind === json.audience_kind && (a.audience_value ?? "") === (json.audience_value ?? ""))
    if (clash) return { __status: 409, code: "operator_conflict", message: `The banner "${clash.title}" already runs for this audience in that time.`, reference: "support_fixture_clash" }
    const fields = { ...json, link_url: json.link_url || undefined, audience_value: json.audience_value || undefined, severity: json.severity ?? "info", dismissible: json.dismissible ?? true }
    if (live) return Object.assign(live, fields, { updated_at: now })
    world.announcements.unshift({ id: `ann_${world.announcements.length + 5}`, ...fields, starts_at: json.starts_at ?? now, created_by: "operator_root", created_at: now, updated_at: now, state: json.starts_at && json.starts_at > now ? "scheduled" : "live", reads: 0, dismissals: 0 })
    return world.announcements[0]
  }
  if (p === "/v1/admin/ip-blocks") {
    world.ipBlocks.unshift({ id: `ipb_${world.ipBlocks.length + 5}`, cidr: json.cidr.includes("/") ? json.cidr : `${json.cidr}/32`, reason: writes.at(-1).reason, created_by: "operator_root", created_at: new Date().toISOString(), ...(json.ends_at ? { expires_at: json.ends_at } : {}), running: true })
    return { resource_id: world.ipBlocks[0].id, state: "blocked" }
  }
  if (area === "ip-blocks" && action === "remove") {
    Object.assign(world.ipBlocks.find((b) => b.id === id), { running: false, revoked_at: new Date().toISOString() })
    return { resource_id: id, state: "lifted" }
  }
  const grantRoute = /^\/v1\/admin\/billing\/workspaces\/([^/]+)\/(grant|revoke)$/.exec(p)
  if (grantRoute) {
    const workspace = decodeURIComponent(grantRoute[1])
    if (grantRoute[2] === "grant") world.grants[workspace] = { id: workspace, scope: "account", scope_id: workspace.split(":")[1], product_code: json.product_code, state: "active", quantity: json.quantity ?? 0, period_end: new Date(Date.now() + json.days * 864e5).toISOString(), cancel_at_period_end: true, granted: true, revision: json.revision + 1, observed_at: new Date().toISOString() }
    else delete world.grants[workspace]
    return { resource_id: workspace, state: grantRoute[2] === "grant" ? "granted" : "revoked", version: json.revision + 1 }
  }
  if (p === "/v1/admin/operators") {
    world.operators.push({ id: "operator_new", email: json.email, role: json.role, status: "active", mfa_enrolled: false, invited: true, live_sessions: 0, created_at: new Date().toISOString() })
    return { resource_id: "operator_new", state: "invited", setup_token: "setup-FIXTURE-token", expires_at: "2026-11-20T00:00:00Z" }
  }
  if (area === "operators") {
    const op = world.operators.find((o) => o.id === id)
    if (!op) return { __status: 404, code: "not_found", message: "The target was not found.", reference: "support_fixture_operator" }
    if (action === "disable") Object.assign(op, { status: "disabled", live_sessions: 0 })
    if (action === "enable") op.status = "active"
    if (action === "mfa-reset") Object.assign(op, { mfa_enrolled: false, live_sessions: 0 })
    if (p.endsWith("/sessions/revoke")) {
      const list = world.operatorSessions[id] ?? []
      world.operatorSessions[id] = json.session_id === "all" ? [] : list.filter((x) => x.id !== json.session_id)
      op.live_sessions = world.operatorSessions[id].length
    }
    return { resource_id: id, state: action }
  }
  if (p === "/v1/admin/selfhost/enrollments") {
    world.installs.push({ installation_id: "inst_1", node_id: "node_s1", endpoint_host: "", name: "", capability: "relay", ready: false, trust_notice: "", scope_kind: "global", scope_id: "", enrollment_state: "pending", enrolledAt: Date.now() })
    return { installations: [] }
  }
  if (area === "selfhost" && method === "DELETE") world.installs = world.installs.filter((i) => i.installation_id !== action)
  if (area === "nodes" && action === "enroll") {
    world.nodes.push({ ...world.nodes[0], id, region_id: json.region_id, roles: json.roles, transports: json.transports, revision: 1 })
    return { runtime_credential: "rc-FIXTURE-credential-value" }
  }
  if (area === "regions" && action === "create") world.regions.push({ id, display_name: json.display_name, enabled: true, revision: 1, updated_at: new Date().toISOString() })
  if (area === "regions" && action === "disable") world.regions.find((r) => r.id === id).enabled = false
  if (p === "/v1/admin/server-nodes/enrollments") world.serverNodes.push({ ...world.serverNodes[0], id: "srv_2", state: "registered", healthy: false })
  if (area === "server-nodes" && action === "drain") world.serverNodes.find((n) => n.id === id).state = "draining"
  if (area === "server-nodes" && action === "resume") {
    const node = world.serverNodes.find((n) => n.id === id)
    if (node.stopped) return { __status: 409, code: "server_node_stopped", message: "The node already shut down after the drain, so there is nothing to resume. Remove it and enrol the slot again.", reference: "support_fixture_stopped" }
    node.state = "active"
  }
  if (area === "server-nodes" && action === "remove") world.serverNodes = world.serverNodes.filter((n) => n.id !== id)
  if (p === "/v1/admin/config/billing") world.policy = { ...json, revision: json.revision + 1 }
  if (p === "/v1/admin/client-update-policy") {
    if (json.expected_revision !== world.floor.revision) return { __status: 409, code: "revision_conflict", message: "The policy changed; reload and try again.", reference: "support_fixture_floor" }
    world.floor = { schema: world.floor.schema, revision: json.expected_revision + 1, minimum_version: json.minimum_version, reason: json.reason }
  }
  if (p === "/v1/admin/release-authority/bundles") return { __status: 403, code: "release_authority_signature_invalid", message: "The bundle does not meet the configured release-authority signing threshold.", reference: "support_fixture_bundle" }
  if (p === "/v1/admin/release-authority/requests") {
    const ok = ["promote", "pause", "quarantine", "revoke"].includes(json.action) && /^\d{4}\.\d+\.\d+\.\d+$/.test(json.version) && json.policy_revision > 0 && (json.action === "promote" ? json.rollout_percentage >= 1 && json.rollout_percentage <= 100 : json.rollout_percentage === 0) && Object.keys(json).length === 7
    if (!ok) return { __status: 400, code: "validation_failed", message: "Release authority request details are invalid.", reference: "support_fixture_request" }
  }
  if (p === "/v1/admin/release-authority/requests") world.requests.push({ id: "req_1", ...json, status: "pending", created_at: new Date().toISOString() })
  return {}
}
function route(req, url) {
  const p = url.pathname
  if (p === "/v1/admin/accounts") {
    const q = (url.searchParams.get("q") ?? "").toLowerCase()
    const status = url.searchParams.get("status")
    const rows = accounts.filter((a) => (!q || a.primary_email.includes(q) || a.id.includes(q)) && (!status || a.status === status))
    return page(rows, "accounts", rows.length, url)
  }
  if (p === "/v1/admin/operators") return page(world.operators, "operators", world.operators.length, url)
  if (/^\/v1\/admin\/operators\/[^/]+\/sessions$/.test(p)) {
    const id = p.split("/")[4]
    const sessions = world.operatorSessions[id] ?? []
    return { operator_id: id, email: world.operators.find((o) => o.id === id)?.email ?? "", total: sessions.length, sessions }
  }
  if (p === "/v1/admin/announcements") {
    const state = url.searchParams.get("state")
    return page(world.announcements.filter((a) => !state || a.state === state), "announcements", undefined, url)
  }
  if (p === "/v1/admin/notices") return page(world.notices, "notices", undefined, url)
  if (p === "/v1/admin/ip-blocks") {
    const state = url.searchParams.get("state")
    return page(world.ipBlocks.filter((b) => !state || (state === "running") === b.running), "blocks", undefined, url)
  }
  if (/^\/v1\/admin\/accounts\/[^/]+\/ips$/.test(p)) {
    const blocked = (ip) => world.ipBlocks.some((b) => b.running && b.cidr.startsWith(ip.split(".").slice(0, 3).join(".")))
    return { account_id: p.split("/")[4], ips: [{ ip: "203.0.113.9", last_seen: "2026-11-17T09:00:00Z", count: 3 }, { ip: "198.51.100.4", last_seen: "2026-11-16T09:00:00Z", count: 1 }].map((v) => ({ ...v, blocked: blocked(v.ip) })) }
  }
  if (p.startsWith("/v1/admin/accounts/")) return accounts.find((a) => a.id === decodeURIComponent(p.split("/").pop())) ?? 404
  const teamBan = () => ({ suspended: Boolean(world.teamBan), generation: world.teamBan ? 3 : 2, ...(world.teamBan ? { ban_reason: world.teamBan.reason, banned_at: world.teamBan.at } : {}) })
  if (p === "/v1/admin/teams") return { teams: [{ id: "team_a", owner_account: "usr_001", ...teamBan(), billing_workspace_id: "bw_t", billing_state: "active" }], total: 1 }
  if (p === "/v1/admin/teams/team_a") return { id: "team_a", owner_account: "usr_001", ...teamBan(), members: [{ account_id: "usr_001", role: "admin", active: true, generation: 1 }], members_total: 1, members_truncated: false, billing_workspace_id: "bw_t", billing_state: "active" }
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
    const all = [...world.auditExtra, ...rows]
    return { events: url.searchParams.get("event_type") === "nothing" ? [] : all, total: url.searchParams.get("event_type") === "nothing" ? 0 : all.length }
  }
  if (p === "/v1/admin/overview")
    return { accounts: [{ state: "active", count: 110 }, { state: "suspended", count: 10 }], billing_workspaces: [{ state: "active", count: 4 }], control_operations: [{ state: "succeeded", count: 9 }], hosts: 7, hosts_ready: 5, hosts_stale: 2, relays: 2, relays_ready: 2, relays_stale: 0 }
  if (p === "/v1/admin/status") return { observed_at: "2026-11-17T09:00:00Z", build: { version: "dev-abc1234", commit: "abc1234def5678abc1234def5678abc1234def56" }, readiness: "ready", dimensions: { database: { status: "healthy" } } }
  if (p === "/v1/admin/analytics/site" || p === "/v1/admin/analytics/console") {
    if (process.env.__FIXTURE_ANALYTICS === "down") return { __status: 503, code: "analytics_unavailable", message: "The analytics provider did not answer.", reference: "support_fixture_posthog" }
    if (process.env.__FIXTURE_ANALYTICS === "off") return { __status: 503, code: "analytics_not_configured", message: "Product analytics is not configured on this server." }
    const base = { observed_at: "2026-11-17T09:00:00Z", days: Number(url.searchParams.get("days") ?? 30), url: "https://posthog.example.test/project/1/web", online: 3 }
    if (p.endsWith("/site"))
      return {
        ...base, visitors: { value: 120, previous: 100 }, signups: { value: 12, previous: 15 },
        daily: [{ day: "2026-11-16", visitors: 30 }, { day: "2026-11-17", visitors: 90 }],
        referrers: [{ name: "news.example.test", visitors: 60, signup_clicks: 5 }, { name: "$direct", visitors: 40, signup_clicks: 2 }], campaigns: [],
        pages: [{ name: "/pricing", count: 90 }], countries: [], errors: { countries: "provider_unavailable" },
      }
    return {
      ...base, daily_active: { value: 4, previous: 3 }, weekly_active: { value: 9, previous: 6 }, paid: { value: 2, previous: 0 }, active_accounts: 20,
      funnel: [{ step: "Signed up", count: 10 }, { step: "Added a machine", count: 6 }, { step: "Used a terminal, command, transfer or tunnel", count: 2 }],
      retention: { size: 8, retained: 5, previous_size: 6, previous_retained: 2 }, adoption: [{ name: "terminal", count: 7 }, { name: "tunnel", count: 2 }],
    }
  }
  if (p.startsWith("/v1/admin/money")) {
    if (process.env.__FIXTURE_MONEY === "off") return { __status: 503, code: "money_not_configured", message: "Stripe is not configured on this server." }
    if (process.env.__FIXTURE_MONEY === "down") return { __status: 503, code: "money_unavailable", message: "Stripe did not answer. Try again in a minute.", reference: "support_fixture_money" }
    const row = (i, extra = {}) => ({ customer: `cus_${i}`, ...(i % 3 ? { workspace: `bw_${i}` } : {}), plan: i % 2 ? "pro" : "team", seats: i, mrr: 1000 * (10 - i), ...extra })
    const rows = { customers: Array.from({ length: 7 }, (_, i) => row(i + 1)), past_due: [row(2, { amount: 2000, attempts: 2, at: 1795000000, state: "past_due" })], renewals: [row(1, { amount: 12000, at: 1795000000 })] }
    if (p === "/v1/admin/money/events") return { events: [{ id: "evt_1", type: "invoice.paid", created: 1795000000, object_id: "in_1" }, { id: "evt_2", type: "charge.failed", created: 1794990000 }] }
    if (p.startsWith("/v1/admin/money/lists/")) {
      const all = rows[p.split("/").pop()]
      return { rows: all, total: all.length }
    }
    return {
      observed_at: "2026-11-17T09:00:00Z", days: Number(url.searchParams.get("days") ?? 30), currency: "usd", partial: false, skipped_currency: 0,
      mrr: { value: 123456, previous: 100000 }, arr: { value: 1481472, previous: 1200000 }, subscribers: { value: 12, previous: 10 },
      trend: [{ day: "2026-11-16", mrr: 100000 }, { day: "2026-11-17", mrr: 123456 }],
      plans: [{ plan: "team", subscriptions: 4, seats: 14, mrr: 90000 }, { plan: "pro", subscriptions: 8, seats: 8, mrr: 33456 }],
      movement: { new: { count: 3, mrr: 30000, previous_count: 1, previous_mrr: 5000 }, churned: { count: 1, mrr: 4000, previous_count: 2, previous_mrr: 9000 }, upgraded: { count: 2, mrr: 6000 }, downgraded: { count: 0, mrr: 0 }, logo_churn_percent: 8.3, previous_logo_churn_percent: 20, revenue_churn_percent: 3.2, previous_revenue_churn_percent: 9, changes_unavailable: false },
      cash: { collected: { value: 150000, previous: 120000 }, refunds: { value: 5000, previous: 0 }, net: { value: 145000, previous: 120000 }, failed_payments: { value: 2, previous: 1 } },
      dunning: { count: 1, at_risk_mrr: 8000, rows: rows.past_due }, customers: { total: 7, rows: rows.customers.slice(0, 5) }, renewals: { total: 1, rows: rows.renewals }, ending: 1,
      errors: { balance_and_payouts: "unauthorized" },
    }
  }
  if (p === "/v1/admin/sentry/issues" && process.env.__FIXTURE_SENTRY === "limited") return { __status: 429, code: "sentry_rate_limited", message: "Sentry is rate limiting requests. Retry later.", reference: "support_fixture_sentry" }
  if (p === "/v1/admin/sentry/issues" && process.env.__FIXTURE_SENTRY === "down") return { __status: 503, code: "operator_evidence_unavailable", message: "Evidence could not be retrieved or recorded.", reference: "support_fixture_sentry" }
  if (p === "/v1/admin/sentry/issues" && process.env.__FIXTURE_SENTRY === "off") return { __status: 503, code: "sentry_not_configured", message: "The Sentry admin API has not been configured." }
  if (p === "/v1/admin/sentry/issues") return { issues: [{ project: "go", id: "1", short_id: "GO-1", level: "error", status: "unresolved", count: 5, first_seen: "2026-11-16T00:00:00Z", last_seen: "2026-11-17T08:00:00Z" }] }
  if (p === "/v1/admin/nodes") return { items: world.nodes, total: world.nodes.length }
  if (p === "/v1/admin/abuse-reports") {
    const all = [
      { id: "abuse_00000000-0000-4000-8000-000000000002", hostname: "bad.example.test", url: "https://bad.example.test/login", reason: "phishing", details: "Asks for bank passwords", reporter_email: "r@example.test", created_at: "2026-11-17T10:00:00Z", status: "open" },
      { id: "abuse_00000000-0000-4000-8000-000000000001", hostname: "spam.example.test", url: "https://spam.example.test/", reason: "spam", details: "", reporter_email: "", created_at: "2026-11-16T10:00:00Z", status: "open" },
      { id: "abuse_00000000-0000-4000-8000-000000000000", hostname: "old.example.test", url: "https://old.example.test/", reason: "other", details: "", reporter_email: "", created_at: "2026-11-01T10:00:00Z", status: "dismissed", triaged_by: "operator_root", triaged_at: "2026-11-02T10:00:00Z", triage_note: "Duplicate" },
    ]
    const reports = all.filter((r) => (!url.searchParams.get("status") || r.status === url.searchParams.get("status")) && (!url.searchParams.get("reason") || r.reason === url.searchParams.get("reason")) && (!url.searchParams.get("hostname") || r.hostname === url.searchParams.get("hostname")))
    return { total: reports.length, reports }
  }
  if (p === "/v1/admin/backups")
    return { database_id: "main", reporting_configured: true, backup_state: "current", restore_state: "stale", observed_at: "2026-11-18T09:00:00Z", max_age_seconds: 93600, restore_max_age_seconds: 691200, evidence_retention_days: 90, latest_backup_restore_verified: false, storage_durability: "deployment_owned_unverified",
      latest_backup: { database_id: "main", run_id: "run-b2", kind: "backup", started_at: "2026-11-18T01:00:00Z", completed_at: "2026-11-18T01:05:00Z", outcome: "succeeded", bytes: 5e9, retained_until: "2026-12-18T00:00:00Z", received_at: "2026-11-18T01:05:01Z" },
      latest_restore: { database_id: "main", run_id: "run-r1", kind: "restore", backup_run_id: "run-b1", started_at: "2026-11-09T02:00:00Z", completed_at: "2026-11-09T02:30:00Z", outcome: "succeeded", bytes: 0, retained_until: "2026-12-09T00:00:00Z", received_at: "2026-11-09T02:30:01Z" } }
  if (p === "/v1/admin/backups/runs")
    return { total: 2, runs: [
      { database_id: "main", run_id: "run-b2", kind: "backup", started_at: "2026-11-18T01:00:00Z", completed_at: "2026-11-18T01:05:00Z", outcome: "succeeded", bytes: 5e9, retained_until: "2026-12-18T00:00:00Z", received_at: "2026-11-18T01:05:01Z" },
      { database_id: "main", run_id: "run-r1", kind: "restore", backup_run_id: "run-b1", started_at: "2026-11-09T02:00:00Z", completed_at: "2026-11-09T02:30:00Z", outcome: "succeeded", bytes: 0, retained_until: "2026-12-09T00:00:00Z", received_at: "2026-11-09T02:30:01Z" } ] }
  if (p === "/v1/admin/publications") {
    const want = url.searchParams.get("blocked")
    const items = [{ hostname: "blk.example.test", blocked: true, legal: false, reason: "TICKET-9", version: 3, created_at: "2026-11-16T00:00:00Z", updated_at: "2026-11-17T00:00:00Z" }, { hostname: "old.example.test", blocked: false, legal: false, reason: "TICKET-8", version: 2, created_at: "2026-11-10T00:00:00Z", updated_at: "2026-11-11T00:00:00Z" }].filter((b) => !want || b.blocked === (want === "true"))
    return { items, total: items.length }
  }
  if (p === "/v1/admin/edge/usage-keys") return { items: [{ key_id: "uk_a", edge_node_id: "edge_1", not_before: "2026-11-01T00:00:00Z", expires_at: "2099-01-01T00:00:00Z", created_at: "2026-11-01T00:00:00Z" }], total: 1 }
  if (p === "/v1/admin/mint/signing-keys") return { items: [{ key_id: "mk_active", active: true, withdrawn: false }, { key_id: "mk_old", active: false, withdrawn: false }], total: 2 }
  if (p === "/v1/admin/regions") return world.regionsFail ? { __status: 503, code: "infrastructure_unavailable", message: "Infrastructure is unavailable.", reference: "support_fixture_regions" } : { items: world.regions, total: world.regions.length }
  if (p === "/v1/admin/selfhost/installations") {
    // A node enrolled a moment ago is waiting; it becomes ready once it has "heartbeated" (2.5 s later).
    for (const i of world.installs) if (i.enrollment_state === "pending" && Date.now() - i.enrolledAt > 2500) Object.assign(i, { enrollment_state: "enrolled", ready: true, endpoint_host: "relay-1.example.test", last_heartbeat_at: new Date().toISOString() })
    const installations = world.installs.map((i) => Object.fromEntries(Object.entries(i).filter(([k]) => k !== "enrolledAt")))
    const count = (f) => installations.filter(f).length
    // Like the server: an opaque cursor for keyset paging, and next_offset (a number) beside it on the first page.
    const limit = Number(url.searchParams.get("limit") ?? 50), cursor = url.searchParams.get("cursor") ?? ""
    if (cursor && !/^c\d+$/.test(cursor)) return { __status: 400, code: "invalid_request", message: "Use the cursor from the previous page." }
    const start = cursor ? Number(cursor.slice(1)) : 0
    const more = start + limit < installations.length
    return { installations: installations.slice(start, start + limit), counts: { pending: count((i) => i.enrollment_state === "pending"), enrolled: count((i) => i.enrollment_state === "enrolled"), ready: count((i) => i.ready) }, pagination: { limit, offset: start, total: installations.length, next_offset: more && !cursor ? start + limit : null, ...(more ? { next_cursor: `c${start + limit}` } : {}) } }
  }
  if (p === "/v1/admin/server-nodes") return { nodes: world.serverNodes, total: world.serverNodes.length }
  if (p === "/v1/admin/config/billing") return world.policy
  if (p === "/v1/admin/client-update-policy") return world.floor
  if (p === "/v1/admin/node-notices") return { notices: [] }
  if (p.startsWith("/v1/admin/release-authority/") && process.env.__FIXTURE_RELEASE === "off") return { __status: 503, code: "release_authority_not_configured", message: "Release authority has not been configured on this server." }
  if (p === "/v1/admin/release-authority/requests") return { requests: world.requests, pagination: { limit: 50, offset: 0, total: world.requests.length, next_offset: null } }
  if (p === "/v1/admin/release-authority/bundles") return { bundles: [], pagination: { limit: 50, offset: 0, total: 0, next_offset: null } }
  if (p === "/v1/admin/infrastructure") return { items: [{ kind: url.searchParams.get("kind"), id: "relay_1", state: "ready", desired_state: "", observed_status: "ready", ready: true, stale: false, revision: 3 }], total: 1 }
  if (p === "/v1/admin/infrastructure/capacity")
    return {
      observed_at: "2026-11-18T09:00:00Z", today: "2026-11-18", month_start: "2026-11-01", total: 4, summary: { edges: 2, relays: 1, servers: 1, attention: 2 },
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
  if (p === "/v1/admin/traffic") {
    // Seeded: 7 relays of 10 report (partial), direct from 3 of 4 active machines; the period decides the bucket.
    const period = url.searchParams.get("period"), hourly = period === "today"
    const at = (i) => hourly ? `2026-11-18T${String(i).padStart(2, "0")}:00:00Z` : `2026-11-${String(10 + i).padStart(2, "0")}T00:00:00Z`
    const series = []
    for (let i = 0; i < 8; i++) for (const [category, base] of [["direct", 4e9], ["relay_managed", 2e9], ["edge_managed", 3e9], ["edge_self_hosted", 5e8]]) series.push({ at: at(i), category, upload_bytes: base * (i + 1), download_bytes: base * (i + 2) })
    const tot = (c) => series.filter((x) => x.category === c).reduce((a, x) => [a[0] + x.upload_bytes, a[1] + x.download_bytes], [0, 0])
    const cover = { direct: [3, 4], relay_managed: [7, 10], relay_self_hosted: [0, 0], edge_managed: [4, 4], edge_self_hosted: [1, 1] }
    return { period, from: "2026-11-01T00:00:00Z", to: "2026-11-18T09:00:00Z", grain: hourly ? "hour" : "day", series,
      totals: Object.keys(cover).map((category) => ({ category, upload_bytes: tot(category)[0], download_bytes: tot(category)[1], reporting: cover[category][0], expected: cover[category][1], coverage_percent: cover[category][1] ? cover[category][0] * 100 / cover[category][1] : null })) }
  }
  if (p === "/v1/admin/traffic/nodes") {
    const kind = url.searchParams.get("kind"), all = Array.from({ length: 60 }, (_, i) => `${kind}-${String(i).padStart(2, "0")}`)
    const start = all.findIndex((id) => id > (url.searchParams.get("after") ?? "")) , limit = Number(url.searchParams.get("limit") ?? 50)
    const items = (start < 0 ? [] : all.slice(start, start + limit)).map((node_id) => ({ node_id, region: "hel", state: "ready", reporting: node_id !== `${kind}-03`, upload_bytes: node_id === `${kind}-03` ? 0 : 1e9, download_bytes: node_id === `${kind}-03` ? 0 : 2e9 }))
    return { period: url.searchParams.get("period"), kind, items, ...(start >= 0 && start + limit < all.length ? { next_after: items.at(-1).node_id } : {}) }
  }
  if (p === "/v1/admin/fleet/versions")
    return {
      observed_at: "2026-11-18T09:00:00Z", machines: 340, online: 301, users: 120, teams: 9, oldest_online_version: "2026.10.28.2",
      platforms: [{ platform: "linux", machines: 200, online: 180, users: 90, teams: 6, oldest_online_version: "2026.10.28.2" }, { platform: "darwin", machines: 100, online: 90, users: 70, teams: 3, oldest_online_version: "2026.11.5.1" }, { platform: "windows", machines: 40, online: 31, users: 20, teams: 2, oldest_online_version: "2026.11.12.1" }],
      versions: [{ platform: "darwin", version: "2026.11.12.1", machines: 70, online: 66, users: 55, teams: 3 }, { platform: "darwin", version: "2026.11.5.1", machines: 30, online: 24, users: 25, teams: 1 }, { platform: "linux", version: "2026.11.12.1", machines: 150, online: 140, users: 80, teams: 5 }, { platform: "linux", version: "2026.10.28.2", machines: 45, online: 40, users: 30, teams: 2 }, { platform: "linux", version: "dev-abc1234", machines: 5, online: 0, users: 2, teams: 0 }, { platform: "windows", version: "2026.11.12.1", machines: 40, online: 31, users: 20, teams: 2 }],
      failures: [{ target_version: "2026.11.12.1", state: "failed", error_code: "signature_invalid", machines: 3 }, { target_version: "2026.11.12.1", state: "rolled_back", error_code: "health_check_failed", machines: 2 }],
    }
  if (p === "/v1/admin/fleet/rollouts")
    return {
      observed_at: "2026-11-18T09:00:00Z",
      rollouts: [{
        version: "2026.11.12.1", platforms: ["darwin", "linux", "windows"], started_at: "2026-11-12T04:30:00Z", action: "promote", rollout_percentage: 100, updated: 260, older: 75, other: 5, failed: 3, rolled_back: 2, offline_since_start: 29, offline_now: 6,
        failures: [{ target_version: "2026.11.12.1", state: "failed", error_code: "signature_invalid", machines: 3 }, { target_version: "2026.11.12.1", state: "rolled_back", error_code: "health_check_failed", machines: 2 }],
        series: [["2026-11-12", 4], ["2026-11-13", 31], ["2026-11-14", 52], ["2026-11-15", 61], ["2026-11-16", 70], ["2026-11-17", 76], ["2026-11-18", 78]].map(([day, share]) => ({ day, total: 335, updated: Math.round((335 * share) / 100), failed: 5 })),
      }, { version: "2026.10.28.2", platforms: ["linux"], started_at: "2026-10-28T04:30:00Z", action: "pause", rollout_percentage: 25, updated: 45, older: 5, other: 0, failed: 0, rolled_back: 0, offline_since_start: 5, offline_now: 0, failures: [], series: [] }],
    }
  if (p === "/v1/admin/fleet/offline") {
    const q = (url.searchParams.get("q") ?? "").toLowerCase(), bucket = url.searchParams.get("bucket"), platform = url.searchParams.get("platform")
    const rows = fleetOffline.filter((m) => (!q || m.id.includes(q) || m.alias.includes(q)) && (!bucket || m.bucket === bucket) && (!platform || m.platform === platform))
    const start = Number(url.searchParams.get("after") ?? 0), limit = Number(url.searchParams.get("limit") ?? 50)
    const count = (b) => fleetOffline.filter((m) => m.bucket === b).length
    return {
      total: rows.length, fleet_machines: 340, offline: fleetOffline.length, items: rows.slice(start, start + limit), ...(start + limit < rows.length ? { next_after: String(start + limit) } : {}),
      buckets: ["under_1_day", "1_to_7_days", "7_to_30_days", "over_30_days", "never_seen"].map((b) => ({ bucket: b, machines: count(b) })),
    }
  }
  const entitlementRead = /^\/v1\/admin\/billing\/workspaces\/([^/]+)\/entitlements$/.exec(p)
  if (entitlementRead) {
    const workspace = decodeURIComponent(entitlementRead[1])
    const grants = world.entitlementGrants[workspace] ?? []
    const running = (kind) => grants.filter((g) => g.kind === kind && g.running).at(-1)
    const team = workspace.startsWith("team:")
    const limit = (kind, used, plan) => ({ kind, used, plan, ...(running(kind) ? { override: running(kind).amount } : {}), limit: running(kind)?.amount ?? (team ? null : plan) })
    const granted = grants.filter((g) => g.kind === "bandwidth" && g.running).reduce((sum, g) => sum + g.amount, 0)
    return {
      workspace_id: workspace,
      bandwidth: { plan_bytes: 5e9, granted_bytes: granted, usage_bytes: 1.2e9 },
      limits: [limit("machines", 2, 100), limit("publications", 1, 100), limit("browser_terminals", 0, 10), limit("public_tcp", 0, 1)],
      ...(team ? { seats: { bought: 3, used: 4, ...(running("seats") ? { override: running("seats").amount } : {}) } } : {}),
      grants: [...grants].reverse(),
    }
  }
  if (p.startsWith("/v1/admin/billing/workspaces/")) {
    const workspace = decodeURIComponent(p.split("/").pop())
    if (workspace === "team:team_a") return { id: workspace, scope: "team", scope_id: "team_a", product_code: "team_monthly", state: "active", quantity: 3, period_end: "2026-12-01T00:00:00Z", cancel_at_period_end: false, granted: false, revision: 1, observed_at: "2026-11-17T09:00:00Z" }
    return world.grants[workspace] ?? 404
  }
  if (p === "/v1/admin/billing/workspaces") return { workspaces: [{ id: "bw_t", scope: "team", scope_id: "team_a", product_code: "team_monthly", state: "active", quantity: 3, period_end: "2026-12-01T00:00:00Z", cancel_at_period_end: false, revision: 1, observed_at: "2026-11-17T09:00:00Z" }], total: 1 }
  if (p === "/v1/admin/billing/workspace-operations") return { operations: [{ id: "op_u1", kind: "checkout", workspace_id: "bw_t", state: "uncertain", updated_at: "2026-11-17T09:00:00Z" }, { id: "op_ok", kind: "checkout", workspace_id: "bw_t", state: "succeeded", updated_at: "2026-11-17T09:00:00Z" }], total: 2 }
  if (p === "/v1/admin/billing/products") return { products: [{ code: "pro_monthly", display_name: "Pro", catalog_type: "personal_subscription", bytes_amount: null, price_id: "price_2", product_id: "prod_2", unit_amount: 900, currency: "usd", interval: "month", allowance_bytes: null, active: true, version: 1, price_verified: true }, { code: "team_monthly", display_name: "Team", catalog_type: "team_subscription", bytes_amount: null, price_id: "price_1", product_id: "prod_1", unit_amount: 1500, currency: "usd", interval: "month", allowance_bytes: null, active: true, version: 1, price_verified: true }] }
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
    if (url.pathname === "/v1/operator/setup") {
      seen.setup = json
      if (json.token !== "setup-FIXTURE-token" || (json.password ?? "").length < 12) return send(400, { error: { code: "invalid_request", message: "Operator setup was rejected.", request_id: "req_setup" } }, { "request-id": "req_setup" })
      sessions["tok-new"].pending = true
      return send(200, { data: { email: json.email, role: "billing", csrf_token: sessions["tok-new"].csrf, mfa: "enroll" } }, { "set-cookie": "pb-operator-session=tok-new; Path=/; HttpOnly; Secure; SameSite=Strict" })
    }
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
    if (session.role === "support" && (["/v1/admin/audit", "/v1/admin/overview", "/v1/admin/operators"].includes(url.pathname) || /^\/v1\/admin\/operators\//.test(url.pathname))) return send(403, { error: { code: "forbidden", message: "This staff role cannot read this resource.", request_id: "req_forbidden" } })
    if (req.method !== "GET" && url.pathname.startsWith("/v1/admin/")) {
      const write = { method: req.method, path: url.pathname, target: req.headers["operator-target"], reason: req.headers["operator-reason"], key: req.headers["idempotency-key"], csrf: req.headers["x-csrf-token"], body: json }
      const policy = expectedTarget(req.method, url.pathname, json)
      if (!policy || !allowedRole(session.role, policy.capability)) return send(403, { error: { code: "forbidden", message: "This staff role cannot perform this action.", request_id: "req_role" } })
      if (write.target !== policy.target || !/^[A-Za-z0-9_.:-]{1,128}$/.test(write.target ?? "") || !/^[A-Za-z0-9_.:-]{1,128}$/.test(write.reason ?? "") || (write.key ?? "").length < 8) return send(400, { error: { code: "validation_failed", message: "Confirm Operator-Target and provide an opaque Operator-Reason reference and Idempotency-Key.", request_id: "req_headers" } })
      writes.push(write)
      const done = mutate(req.method, url.pathname, json)
      if (done.__status) return send(done.__status, { error: { code: done.code, message: done.message, request_id: "req_x", support_reference: done.reference } }, { "support-reference": done.reference ?? "" })
      return send(200, { data: done })
    }
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
    if (process.env.CHECK_DEBUG) console.error(`--- ${name}\n${error.stack ?? error}`)
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
  // The checks poll with waitForFunction, which the production CSP (no unsafe-eval) would block; the CSP is not under test here.
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 }, bypassCSP: true })
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
  await step("overview shows key numbers, control plane status and the week's analytics", async () => {
    await p.goto(`${ORIGIN}/overview`)
    await p.waitForSelector("[data-overview-ready]")
    expect((await text('[data-stat="Accounts"]')).includes("120"), "accounts total")
    await p.waitForSelector('[data-analytics-state="ready"]')
    expect((await text('[data-stat="Visitors"]')).includes("↑ 20%"), "visitors change against the previous range")
    expect((await text("[data-build]")).includes("dev-abc1234"), "server build version not shown")
    await p.screenshot({ path: `${shots}/overview-light.png`, fullPage: true })
  })
  await step("money: MRR, movement, cash, lists with View all, a failing section named, events paged", async () => {
    await p.goto(`${ORIGIN}/money`)
    await p.waitForSelector('[data-money-state="ready"]')
    expect((await text('[data-stat="MRR"]')).includes("$1,234.56") && (await text('[data-stat="MRR"]')).includes("↑ 23%"), "MRR with its change")
    expect((await text('[data-stat="ARR"]')).includes("$14,814.72"), "ARR")
    const movement = await text("[data-money-movement]")
    expect(movement.includes("Churn rate") && movement.includes("8.3%") && movement.includes("↓ 11.7 pts") && !movement.includes("changes_unavailable"), `movement: ${JSON.stringify(movement)}`)
    expect((await text("[data-money-cash]")).includes("$1,450.00"), "net revenue")
    expect((await text("[data-insight-error=\"balance_and_payouts\"]")).includes("Stripe refused the API key"), "failing section named in words")
    expect(await p.locator('[data-money-list="customers"] tbody tr').count() === 5, "top customers shows five rows")
    expect((await text('[data-money-list="past_due"]')).includes("$20.00"), "failed payment amount")
    await p.screenshot({ path: `${shots}/money-light.png`, fullPage: true })
    await p.getByRole("button", { name: "View all 7" }).click()
    await p.waitForSelector('[data-table="the list"][data-state="ready"]')
    expect(await p.locator('[data-table="the list"] tbody tr').count() === 7, "view all lists every customer")
    await p.screenshot({ path: `${shots}/money-view-all-light.png`, fullPage: true })
    await p.keyboard.press("Escape")
    await p.getByRole("tab", { name: "Stripe events" }).click()
    await waitReady("Stripe events")
    expect(await p.locator('[data-table="Stripe events"] tbody tr').count() === 2, "events listed")
  })
  await step("analytics: website numbers with changes, hidden empty widgets, then console funnel, retention and feature use", async () => {
    await p.goto(`${ORIGIN}/analytics`)
    await p.waitForSelector('[data-analytics-app="site"]')
    const site = await text('[data-analytics-app="site"]')
    expect(site.includes("↓ 20%") && site.includes("10.0% of visitors"), "signups change and conversion")
    expect(site.includes("news.example.test") && site.includes("Direct") && site.includes("/pricing"), "referrers and pages")
    expect(!site.includes("Campaigns") && !site.includes("Countries"), "empty widgets are hidden")
    expect((await text('[data-insight-error="countries"]')).includes("Couldn’t load countries. PostHog is down") && (await text('[data-insight-error="countries"]')).includes("provider_unavailable"), "partial insight failure is named in words with its code")
    expect(site.includes("3 online now") && site.includes("Open in PostHog"), "online count, PostHog link")
    await p.screenshot({ path: `${shots}/analytics-site-light.png`, fullPage: true })
    await p.getByRole("button", { name: "Console" }).click()
    await p.waitForSelector('[data-analytics-app="console"]')
    const product = await text('[data-analytics-app="console"]')
    expect(product.includes("Weekly active") && product.includes("↑ 50%") && product.includes("63%") && product.includes("↑ 30 pts"), "active accounts and week-1 retention")
    expect((await text("[data-dropoff]")).includes("Used a terminal, command, transfer or tunnel"), "the step with the largest loss is highlighted")
    expect((await text("[data-adoption]")).includes("Terminals") && (await text("[data-adoption]")).includes("35%"), "feature use as a share of active accounts")
    await p.screenshot({ path: `${shots}/analytics-console-light.png`, fullPage: true })
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
    expect(await p.getByText("No users match.").isVisible(), "empty text")
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
  for (const [path, what] of [["/teams", "teams"], ["/machines", "machines"], ["/audit", "audit events"], ["/sentry", "errors"]]) {
    await step(`${path} loads rows`, async () => {
      await p.goto(`${ORIGIN}${path}`)
      await waitReady(what)
      expect(await p.locator(`[data-table="${what}"] tbody tr`).count() > 0, "no rows")
    })
  }
  await step("an unconfigured service says so, without Retry", async () => {
    process.env.__FIXTURE_SENTRY = "off"
    try {
      await p.goto(`${ORIGIN}/sentry`)
      await p.waitForSelector("[data-error-block]")
      const shown = await text("[data-error-block]")
      expect(shown.includes("has not been configured") && !shown.includes("Retry"), `shown: ${shown}`)
    } finally {
      delete process.env.__FIXTURE_SENTRY
    }
  })
  // Every optional integration in both states: unset says so in one line (no Retry); down names the provider with its reference and a Retry.
  for (const [name, path, env, down, unset, ref] of [
    ["Stripe", "/money", "__FIXTURE_MONEY", "Stripe isn’t answering.", "Stripe isn’t set up on this server", "support_fixture_money"],
    ["PostHog", "/analytics", "__FIXTURE_ANALYTICS", "PostHog isn’t answering.", "PostHog isn’t set up on this server", "support_fixture_posthog"],
    ["Sentry", "/sentry", "__FIXTURE_SENTRY", "Sentry isn’t answering.", "has not been configured", "support_fixture_sentry"],
  ]) {
    await step(`${name}: down shows a short error with its reference; not configured says so in one line`, async () => {
      try {
        process.env[env] = "down"
        await p.goto(`${ORIGIN}${path}`)
        await p.waitForSelector("[data-error-block]")
        const shown = await text("[data-error-block]")
        expect(shown.includes(down) && shown.includes(ref) && shown.includes("Retry") && !shown.includes("Couldn’t load"), `down shown: ${shown}`)
        process.env[env] = "off"
        await p.goto(`${ORIGIN}${path}`)
        await p.waitForSelector(`[data-error-block], [data-money-state="not-configured"], [data-analytics-state="not-configured"]`)
        const line = await text("main")
        expect(line.includes(unset) && !line.includes("Retry") && !line.includes("Couldn’t load"), `unset shown: ${line}`)
      } finally {
        delete process.env[env]
      }
    })
  }
  await step("Sentry rate limited: says so (not an outage), with Retry and its reference", async () => {
    process.env.__FIXTURE_SENTRY = "limited"
    try {
      await p.goto(`${ORIGIN}/sentry`)
      await p.waitForSelector("[data-error-block]")
      const shown = await text("[data-error-block]")
      expect(shown.includes("rate limiting") && !shown.includes("isn’t answering") && shown.includes("Retry") && shown.includes("support_fixture_sentry"), `shown: ${shown}`)
    } finally {
      delete process.env.__FIXTURE_SENTRY
    }
  })
  await step("release authority not configured: one line, no buttons, no error panels", async () => {
    process.env.__FIXTURE_RELEASE = "off"
    try {
      await p.goto(`${ORIGIN}/releases`)
      await p.getByRole("tab", { name: "Release authority" }).click()
      await p.waitForSelector('[data-release-authority-state="not-configured"]')
      expect(await p.locator("[data-error-block]").count() === 0, "error panels shown for an unset integration")
      expect((await text("main")).includes("Release authority isn’t set up"), "line missing")
    } finally {
      delete process.env.__FIXTURE_RELEASE
    }
  })
  await step("audit: a filter with no events shows the empty state", async () => {
    await p.goto(`${ORIGIN}/audit`)
    await p.fill('[aria-label="Event type"]', "nothing")
    await p.waitForSelector('[data-table="audit events"][data-state="empty"]')
  })
  await step("relays & edges: nodes load; regions failure shows its reference", async () => {
    await p.goto(`${ORIGIN}/infrastructure`)
    await p.getByRole("tab", { name: "Managed nodes" }).click()
    await waitReady("nodes")
    await p.getByRole("tab", { name: "Regions" }).click()
    await p.waitForSelector("[data-error-block]")
    expect((await text("[data-error-block]")).includes("support_fixture_regions"), "reference missing")
    await p.getByRole("tab", { name: "Relay health" }).click()
    await waitReady("relay health")
    world.regionsFail = false
  })
  // ---- writes -----------------------------------------------------------------------------------
  const dialog = (title) => p.locator(`[data-action-dialog="${title}"]`)
  // A row's "…" menu, then one of its items (each opens its confirmation).
  const rowAction = async (scope, item) => {
    await p.locator(`${scope} [data-row-menu]`).click()
    await p.getByRole("menuitem", { name: `${item}…`, exact: true }).click()
  }
  const lastWrite = () => writes.at(-1)
  const submit = async (d, button, reason = "TICKET-42") => {
    await d.getByLabel("Reference", { exact: true }).fill(reason)
    await d.getByRole("button", { name: button, exact: true }).click()
  }
  await step("enrol a node: command is exact, a bad reference blocks submit, the one-time code goes with target/reason/key, the node goes waiting then ready", async () => {
    await p.goto(`${ORIGIN}/infrastructure`)
    await p.waitForSelector("[data-enrol-node]")
    await p.getByLabel("Public hostname or IP").fill("relay-1.example.test")
    await p.getByLabel("Label", { exact: true }).fill("Hetzner Helsinki")
    expect((await text("[data-install-command]")) === "sudo pbh install --relay --endpoint-host relay-1.example.test --region selfhost --name 'Hetzner Helsinki'", `command was ${await text("[data-install-command]")}`)
    await p.getByRole("button", { name: "I ran it, enrol the node" }).click()
    const d = dialog("Enrol the node")
    await d.waitFor()
    const code = `pbh1.${"a".repeat(43)}.${"b".repeat(43)}`
    await d.getByLabel("Self-host code").fill("pbh1.too-short")
    await d.getByLabel("Reference", { exact: true }).fill("bad reason!")
    expect(await d.getByRole("button", { name: "Enrol node", exact: true }).isDisabled(), "submit must be disabled for a malformed code and reference")
    await d.getByLabel("Self-host code").fill(code)
    await submit(d, "Enrol node")
    await p.waitForSelector('[data-installation="inst_1"]')
    const w = lastWrite()
    expect(w.method === "POST" && w.path === "/v1/admin/selfhost/enrollments" && w.target === "relay-1.example.test:8443" && w.reason === "TICKET-42" && w.key.length >= 8 && w.body.code === code, `write was ${JSON.stringify({ ...w, body: "…" })}`)
    expect(w.csrf === "csrf-admin", "the relay did not add the CSRF token")
    expect((await text("[data-installation-counts]")).includes("1 waiting"), "a new node is waiting")
    await p.waitForFunction(() => document.querySelector("[data-installation-counts]")?.textContent?.includes("1 ready"), null, { timeout: 20000 })
    await p.screenshot({ path: `${shots}/enrol-light.png`, fullPage: true })
  })
  await step("managed node: the runtime credential is shown once and must be acknowledged", async () => {
    await p.getByRole("tab", { name: "Managed nodes" }).click()
    await waitReady("nodes")
    await p.getByRole("button", { name: "Enrol a managed node" }).click()
    const d = dialog("Enrol a managed node")
    await d.getByLabel("Node id").fill("relay-sin-1")
    await d.getByLabel("Kind").selectOption("relay")
    await d.getByLabel("Region", { exact: true }).selectOption("eu")
    await d.getByLabel("Pool").fill("pool-a")
    await d.getByLabel("Failure domain").fill("rack-1")
    await d.getByLabel("Public hostname").fill("relay-sin-1.example.test")
    await d.getByLabel("QUIC port").fill("27446")
    await submit(d, "Enrol node")
    await p.waitForSelector("[data-one-time-secrets]")
    expect((await text("[data-secret]")) === "rc-FIXTURE-credential-value", "credential not shown")
    const w = lastWrite()
    expect(w.path === "/v1/admin/nodes/relay-sin-1/enroll" && w.target === "relay-sin-1" && w.body.revision === 0 && w.body.roles.join() === "relay" && w.body.transports.join() === "derp_quic,derp_wss", `managed enrol body ${JSON.stringify(w.body)}`)
    expect(await p.getByRole("button", { name: "Done" }).isDisabled(), "Done must wait for the acknowledgement")
    await p.getByText("I saved it").click()
    await p.getByRole("button", { name: "Done" }).click()
    await p.waitForFunction(() => !document.querySelector("[data-one-time-secrets]"))
    expect(!(await p.content()).includes("rc-FIXTURE-credential-value"), "the credential stayed on the page")
  })
  await step("managed node: drain and retire need a reference; retire needs the node id typed", async () => {
    await rowAction('[data-node-actions="node_1"]', "Drain")
    await submit(dialog("Drain node_1"), "Drain")
    await p.waitForFunction(() => !document.querySelector('[data-action-dialog="Drain node_1"]'))
    expect(lastWrite().path === "/v1/admin/nodes/node_1/drain" && lastWrite().body.revision === 1, "drain write")
    await rowAction('[data-node-actions="node_1"]', "Retire")
    const d = dialog("Retire node_1")
    await d.getByLabel("Reference", { exact: true }).fill("TICKET-43")
    expect(await d.getByRole("button", { name: "Retire", exact: true }).isDisabled(), "retire must need the typed id")
    await d.getByLabel(/Type/).fill("node_1")
    await d.getByRole("button", { name: "Retire", exact: true }).click()
    await p.waitForFunction(() => !document.querySelector('[data-action-dialog="Retire node_1"]'))
    expect(lastWrite().path === "/v1/admin/nodes/node_1/retire", "retire write")
  })
  await step("managed node: editing keeps the enrolled usage key, so there is nothing to paste and none is sent", async () => {
    await p.getByRole("tab", { name: "Managed nodes" }).click()
    await waitReady("nodes")
    await rowAction('[data-node-actions="edge_1"]', "Edit")
    const d = dialog("Update edge_1")
    expect((await d.getByLabel(/Usage public key/).count()) === 0, "the edit form asks for the usage key")
    expect((await d.innerText()).includes("kept from enrolment"), "the hint does not say the key is kept")
    await d.getByLabel("Public hostname").fill("edge-1b.example.test")
    await submit(d, "Save configuration")
    await p.waitForFunction(() => !document.querySelector('[data-action-dialog="Update edge_1"]'))
    const w = lastWrite()
    expect(w.path === "/v1/admin/nodes/edge_1/update" && w.body.revision === 4 && w.body.endpoint_host === "edge-1b.example.test" && !("usage_public_key" in w.body), `update body ${JSON.stringify(w.body)}`)
  })
  await step("regions: create, and disable asks for the region id", async () => {
    await p.getByRole("tab", { name: "Regions" }).click()
    await waitReady("regions")
    await p.getByRole("button", { name: "Create a region" }).click()
    const d = dialog("Create a region")
    await d.getByLabel("Region id").fill("sin")
    await d.getByLabel("Display name").fill("Singapore")
    await submit(d, "Create region")
    await p.waitForSelector('[data-region-actions="sin"]')
    expect(lastWrite().path === "/v1/admin/regions/sin/create" && lastWrite().target === "sin", "create write")
    await rowAction('[data-region-actions="eu"]', "Disable")
    const x = dialog("Disable eu")
    await x.getByLabel("Reference", { exact: true }).fill("TICKET-44")
    expect(await x.getByRole("button", { name: "Disable region" }).isDisabled(), "disable must need the typed id")
    await x.getByLabel(/Type/).fill("eu")
    await x.getByRole("button", { name: "Disable region" }).click()
    await p.waitForSelector('[data-region-actions="eu"][data-actions~="enable"]')
    expect(lastWrite().path === "/v1/admin/regions/eu/disable" && lastWrite().body.revision === 2, "disable write")
  })
  await step("self-hosted nodes page with the server's cursor", async () => {
    const saved = world.installs
    world.installs = [...saved, ...Array.from({ length: 55 }, (_, n) => ({ ...saved[0], installation_id: `inst_x${n}`, node_id: `node_x${n}`, name: `extra ${n}` }))]
    await p.getByRole("tab", { name: "Self-hosted" }).click()
    await p.waitForSelector('[data-table="installed nodes"] [data-list-range]')
    expect((await text('[data-table="installed nodes"] [data-list-range]')).replace(/\s+/g, " ").startsWith("1–50 of"), "first self-hosted page range")
    await p.locator('[data-table="installed nodes"]').getByRole("button", { name: "Next" }).click()
    await p.waitForFunction(() => document.querySelector('[data-table="installed nodes"] [data-list-range]')?.textContent?.startsWith("51"))
    world.installs = saved
  })
  await step("abuse reports filter on the server, open in full, and take a hostname down", async () => {
    await p.goto(`${ORIGIN}/abuse`)
    await waitReady("abuse reports")
    expect((await p.locator('[data-table="abuse reports"] tbody tr').count()) === 2, "the open queue lists the two open reports, not the dismissed one")
    await p.getByRole("button", { name: "Remove Status filter" }).click()
    await p.waitForFunction(() => document.querySelectorAll('[data-table="abuse reports"] tbody tr').length === 3)
    await p.click("[data-filter-button]")
    await p.getByRole("menuitem", { name: "Status" }).click()
    await p.getByRole("menuitemradio", { name: "Open", exact: true }).click()
    await p.waitForFunction(() => document.querySelectorAll('[data-table="abuse reports"] tbody tr').length === 2)
    await rowAction('[data-abuse-actions="abuse_00000000-0000-4000-8000-000000000001"]', "Dismiss report")
    const dismiss = dialog("Dismiss report")
    await dismiss.getByLabel("Note").fill("Not ours")
    await submit(dismiss, "Dismiss report")
    await p.waitForFunction(() => !document.querySelector('[data-action-dialog="Dismiss report"]'))
    expect(lastWrite().path === "/v1/admin/abuse-reports/abuse_00000000-0000-4000-8000-000000000001/triage" && lastWrite().target === "abuse_00000000-0000-4000-8000-000000000001" && lastWrite().body.status === "dismissed" && lastWrite().body.note === "Not ours", `triage write ${JSON.stringify(lastWrite())}`)
    await p.click("[data-filter-button]")
    await p.getByRole("menuitem", { name: "Reason" }).click()
    await p.getByRole("menuitemradio", { name: "Spam", exact: true }).click()
    await p.waitForFunction(() => document.querySelectorAll('[data-table="abuse reports"] tbody tr').length === 1)
    await p.getByRole("button", { name: "Remove Reason filter" }).click()
    await p.waitForFunction(() => document.querySelectorAll('[data-table="abuse reports"] tbody tr').length === 2)
    await rowAction('[data-abuse-actions="abuse_00000000-0000-4000-8000-000000000002"]', "View")
    await p.waitForFunction(() => document.querySelector('[role="dialog"]')?.textContent?.includes("Asks for bank passwords"))
    await p.keyboard.press("Escape")
    await rowAction('[data-abuse-actions="abuse_00000000-0000-4000-8000-000000000002"]', "Block hostname")
    const d = dialog("Block bad.example.test")
    await d.getByLabel(/Type/).fill("bad.example.test")
    await submit(d, "Block hostname", "abuse_00000000-0000-4000-8000-000000000002")
    await p.waitForFunction(() => !document.querySelector('[data-action-dialog="Block bad.example.test"]'))
    expect(lastWrite().path === "/v1/admin/publications/bad.example.test/block" && lastWrite().body.revision === 1 && lastWrite().target === "bad.example.test" && lastWrite().reason === "abuse_00000000-0000-4000-8000-000000000002", `takedown write ${JSON.stringify(lastWrite())}`)
  })
  await step("backups show the evidence state, the latest restore test and every run", async () => {
    await p.goto(`${ORIGIN}/backups`)
    await p.waitForSelector("[data-backup-status]")
    expect((await p.locator('[data-backup-state="current"]').count()) === 1 && (await p.locator('[data-backup-state="stale"]').count()) === 1, "backup and restore states")
    expect((await text("[data-backup-status]")).includes("run-r1"), "the latest restore test is shown")
    await waitReady("backup runs")
    expect((await p.locator('[data-table="backup runs"] tbody tr').count()) === 2, "two runs listed")
  })
  await step("keys and hostname blocks are lists: rows revoke or unblock with their own id and revision", async () => {
    await p.goto(`${ORIGIN}/infrastructure`)
    await p.getByRole("tab", { name: "Keys" }).click()
    await waitReady("usage keys")
    await p.waitForSelector('[data-table="signing keys"] [data-signing-key-actions="mk_old"]')
    expect((await p.locator('[data-signing-key-actions="mk_active"]').count()) === 0, "the active signing key offers revoke")
    await rowAction('[data-usage-key-actions="uk_a"]', "Revoke")
    const u = dialog("Revoke usage key uk_a")
    await u.getByLabel("Revocation reason").fill("leaked")
    await u.getByLabel(/Type/).fill("uk_a")
    await submit(u, "Revoke key")
    await p.waitForFunction(() => !document.querySelector('[data-action-dialog="Revoke usage key uk_a"]'))
    expect(lastWrite().path === "/v1/admin/edge/usage-keys/uk_a/revoke" && lastWrite().target === "uk_a" && lastWrite().reason === "TICKET-42" && lastWrite().body.reason === "leaked", `usage key revoke ${JSON.stringify(lastWrite())}`)
    await p.getByRole("tab", { name: "Hostnames" }).click()
    await waitReady("hostname blocks")
    expect((await p.locator('[data-block-actions="old.example.test"]').getAttribute("data-actions")) === "block", "an unblocked name offers Block again")
    await p.click("[data-filter-button]")
    await p.getByRole("menuitem", { name: "State" }).click()
    await p.getByRole("menuitemradio", { name: "Blocked", exact: true }).click()
    await p.waitForFunction(() => !document.querySelector('[data-block-actions="old.example.test"]') && document.querySelector('[data-filter-chip="State"]'))
    await p.getByRole("button", { name: "Remove State filter" }).click()
    await p.waitForSelector('[data-block-actions="old.example.test"]')
    await rowAction('[data-block-actions="blk.example.test"]', "Unblock")
    await submit(dialog("Unblock blk.example.test"), "Unblock hostname")
    await p.waitForFunction(() => !document.querySelector('[data-action-dialog="Unblock blk.example.test"]'))
    expect(lastWrite().path === "/v1/admin/publications/blk.example.test/unblock" && lastWrite().target === "blk.example.test" && lastWrite().reason === "TICKET-42" && lastWrite().body.revision === 3, `unblock write ${JSON.stringify(lastWrite())}`)
  })
  await step("server nodes: the enrol command is exact; enrol, drain and remove are writes", async () => {
    await p.getByRole("tab", { name: "Server nodes" }).click()
    await p.waitForSelector("[data-enrol-server-node]")
    await p.getByLabel("Region", { exact: true }).fill("hel")
    await p.getByLabel("Hostname").fill("server-3.example.test")
    expect((await text("[data-enrol-server-node] [data-install-command]")) === "paperboat-server enroll-node -region hel -instance slot-01 -host server-3.example.test -config-out /etc/paperboat/node.json", "server-node command")
    await p.getByRole("button", { name: "I ran it, enrol the node" }).click()
    const d = dialog("Enrol the server node")
    await d.getByLabel("Claim code").fill("pbs1.fixture-code")
    await submit(d, "Enrol server node")
    await p.waitForSelector('[data-server-node="srv_2"]')
    expect(lastWrite().path === "/v1/admin/server-nodes/enrollments" && lastWrite().target === "server-3.example.test:8443", "server enrol write")
    await rowAction('[data-server-node-actions="srv_2"]', "Drain")
    const drain = dialog("Drain srv_2")
    await drain.getByLabel("Reference", { exact: true }).fill("TICKET-44b")
    expect(await drain.getByRole("button", { name: "Drain node" }).isDisabled(), "drain is one-way and must need the typed id")
    await drain.getByLabel(/Type/).fill("srv_2")
    await drain.getByRole("button", { name: "Drain node" }).click()
    await p.waitForSelector('[data-server-node-actions="srv_2"][data-actions~="resume"]')
    expect(!(await p.locator('[data-server-node-actions="srv_2"]').getAttribute("data-actions")).includes("drain"), "a draining node offers Resume, not Drain")
    expect(lastWrite().path === "/v1/admin/server-nodes/srv_2/drain", "drain write")
    await rowAction('[data-server-node-actions="srv_2"]', "Resume")
    await submit(dialog("Resume srv_2"), "Resume node")
    await p.waitForSelector('[data-server-node-actions="srv_2"][data-actions~="drain"]')
    expect(lastWrite().path === "/v1/admin/server-nodes/srv_2/resume" && lastWrite().target === "srv_2", "resume write")
    // A drained node whose process has exited cannot be resumed: the server says so and the panel shows it with its reference.
    world.serverNodes.find((n) => n.id === "srv_2").stopped = true
    await rowAction('[data-server-node-actions="srv_2"]', "Drain")
    const again = dialog("Drain srv_2")
    await again.getByLabel("Reference", { exact: true }).fill("TICKET-44c")
    await again.getByLabel(/Type/).fill("srv_2")
    await again.getByRole("button", { name: "Drain node" }).click()
    await p.waitForSelector('[data-server-node-actions="srv_2"][data-actions~="resume"]')
    await rowAction('[data-server-node-actions="srv_2"]', "Resume")
    const stale = dialog("Resume srv_2")
    await submit(stale, "Resume node")
    await stale.locator("[data-action-error]").waitFor()
    expect((await stale.locator("[data-action-error]").innerText()).includes("already shut down") && (await stale.locator("[data-action-error]").innerText()).includes("support_fixture_stopped"), "the stopped-node answer is not shown with its reference")
    await stale.getByRole("button", { name: "Cancel" }).click()
    await rowAction('[data-server-node-actions="srv_1"]', "Remove")
    const r = dialog("Remove srv_1")
    await r.getByLabel("Reference", { exact: true }).fill("TICKET-45")
    await r.getByLabel(/Type/).fill("srv_1")
    await r.getByRole("button", { name: "Remove node" }).click()
    await p.waitForFunction(() => !document.querySelector('[data-server-node="srv_1"]'))
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
  await step("traffic: five totals, split bar, partial coverage labelled, period picker, paged node tables", async () => {
    await p.goto(`${ORIGIN}/traffic`)
    await p.waitForSelector("[data-traffic-ready]")
    for (const label of ["Direct", "Our relays", "Self-hosted relays", "Our edges", "Self-hosted edges"]) expect((await p.locator(`[data-stat="${label}"]`).count()) === 1, `${label} total`)
    expect((await text('[data-coverage="relay_managed"]')).includes("Partial") && (await text('[data-coverage="relay_managed"]')).includes("7 of 10"), "partial relay coverage is labelled")
    expect((await text('[data-coverage="edge_managed"]')).includes("4 of 4") && !(await text('[data-coverage="edge_managed"]')).includes("Partial"), "complete edge coverage is not labelled partial")
    expect((await text('[data-coverage="relay_self_hosted"]')).includes("No nodes expected"), "no self-hosted relays expected")
    expect((await p.locator("[data-split] [title]").count()) >= 3, "split bar segments")
    expect((await p.locator('[data-series="ready"] li').count()) === 8, "eight hourly buckets today")
    await p.getByRole("button", { name: "7 days" }).click()
    await p.waitForFunction(() => document.querySelector("[data-series=ready]")?.getAttribute("aria-label")?.includes("per day"))
    await p.waitForSelector("[data-table=traffic-relay] tbody tr")
    expect((await p.locator("[data-table=traffic-relay] tbody tr").count()) === 50, "first page of relays")
    expect((await text('[data-table=traffic-relay] [data-node="relay-03"]')).includes("no report in this period"), "a silent node is flagged")
    await p.locator("[data-table=traffic-relay]").locator("xpath=ancestor::section").getByRole("button", { name: "Next" }).click()
    await p.waitForFunction(() => document.querySelectorAll("[data-table=traffic-relay] tbody tr").length === 10)
    await p.screenshot({ path: `${shots}/traffic-light.png`, fullPage: true })
  })
  await step("fleet: versions per OS, rollout progress with its chart, and the offline list with real totals in IST", async () => {
    await p.goto(`${ORIGIN}/fleet`)
    await p.waitForSelector("[data-fleet-versions-ready]")
    expect((await text('[data-stat="Machines"]')).includes("340"), "machines")
    expect((await text('[data-stat="Oldest version online"]')).includes("2026.10.28.2"), "oldest online version")
    expect((await p.locator("[data-table=fleet-versions] tbody tr").count()) === 6, "six version rows")
    expect((await text("[data-table=fleet-failures]")).includes("signature invalid"), "failure reason")
    await p.screenshot({ path: `${shots}/fleet-versions-light.png`, fullPage: true })
    await p.getByRole("tab", { name: "Rollouts" }).click()
    await p.waitForSelector("[data-fleet-rollouts-ready]")
    expect((await text('[data-rollout="2026.11.12.1"] [data-stat="Updated"]')).includes("77.6%"), "updated share is 260 of 335")
    expect((await text('[data-rollout="2026.11.12.1"] [data-stat="Offline the whole time"]')).includes("29"), "offline for the whole window")
    expect((await p.locator('[data-rollout-chart="2026.11.12.1"]').count()) === 1, "rollout chart")
    expect((await p.getByText("Paused", { exact: true }).count()) === 1, "a paused rollout is labelled paused")
    expect(await p.getByText("IST").first().isVisible(), "rollout start in IST")
    await p.screenshot({ path: `${shots}/fleet-rollouts-light.png`, fullPage: true })
    await p.getByRole("tab", { name: "Offline" }).click()
    await p.waitForSelector("[data-fleet-offline-ready]")
    await waitReady("fleet-offline")
    expect((await text("[data-list-range]")) === "1–50 of 120", "first page of 120")
    expect((await text('[data-bucket="over_30_days"]')).includes("24"), "bucket count")
    expect((await text("[data-table=fleet-offline] tbody tr")).includes("IST"), "last seen in IST")
    await p.getByRole("button", { name: "Next" }).click()
    await p.waitForFunction(() => document.querySelector("[data-list-range]")?.textContent?.startsWith("51"))
    await p.screenshot({ path: `${shots}/fleet-offline-light.png`, fullPage: true })
    await p.locator('[data-bucket="never_seen"]').click()
    await p.waitForFunction(() => document.querySelector("[data-list-range]")?.textContent === "1–24 of 24" || document.querySelector("[data-list-range]")?.textContent === "24 total")
    expect((await text("[data-table=fleet-offline] tbody tr")).includes("Never"), "never-seen machines say so")
  })
  await step("billing: workspaces, operations and plans", async () => {
    await p.goto(`${ORIGIN}/billing`)
    await waitReady("billing workspaces")
    await p.getByRole("tab", { name: "Operations" }).click()
    await waitReady("billing operations")
    await p.getByRole("tab", { name: "Plans" }).click()
    await waitReady("plans and prices")
    expect(await p.getByText("15.00 USD / month").isVisible(), "price")
  })
  await step("billing: product edit and policy change are fenced writes; recovery needs evidence and the operation id", async () => {
    await p.goto(`${ORIGIN}/billing`)
    await p.getByRole("tab", { name: "Plans" }).click()
    await p.waitForSelector("[data-billing-policy]")
    await waitReady("plans and prices")
    await p.locator('[data-product-action="team_monthly"]').click()
    const d = dialog("Edit team_monthly")
    await d.getByLabel("Display name").fill("Team plan")
    await submit(d, "Save product")
    await p.waitForFunction(() => !document.querySelector('[data-action-dialog="Edit team_monthly"]'))
    const w = lastWrite()
    expect(w.method === "PUT" && w.path === "/v1/admin/billing/products/team_monthly" && w.body.version === 1 && w.body.provider_price_id === "price_1" && w.body.catalog_type === "team_subscription" && w.body.display_name === "Team plan", `product write ${JSON.stringify(w.body)}`)
    await p.getByRole("button", { name: "Change", exact: true }).click()
    const q = dialog("Change the monthly allowances")
    await q.getByLabel("Team allowance (bytes)").fill("20000000000")
    await submit(q, "Save allowances")
    await p.waitForSelector('[data-billing-policy][data-revision="4"]')
    expect(lastWrite().body.revision === 3 && lastWrite().body.team_allowance_bytes === 20000000000 && lastWrite().body.personal_allowance_bytes === 5e9, "policy write")
    await p.getByRole("tab", { name: "Operations" }).click()
    await waitReady("billing operations")
    expect((await p.locator('[data-operation-actions="op_ok"]').count()) === 0, "a succeeded operation must offer no recovery")
    await p.locator('[data-operation-actions="op_u1"]').getByRole("button", { name: "Retire" }).click()
    const r = dialog("Retire as not applied")
    await r.getByLabel("Evidence reference").fill("short")
    await r.getByLabel("Reference", { exact: true }).fill("TICKET-46")
    await r.getByLabel(/Type/).fill("op_u1")
    expect(await r.getByRole("button", { name: "Retire operation" }).isDisabled(), "evidence under 8 characters must block")
    await r.getByLabel("Evidence reference").fill("evt_stripe_123")
    await r.getByRole("button", { name: "Retire operation" }).click()
    await p.waitForFunction(() => !document.querySelector('[data-action-dialog="Retire as not applied"]'))
    expect(lastWrite().path === "/v1/admin/billing/workspace-operations/op_u1/retire" && lastWrite().body.outcome === "not_applied" && lastWrite().body.evidence_reference === "evt_stripe_123", "retire write")
  })
  await step("releases: floor needs a valid version and the typed word; a stale revision shows its reference; a refused bundle keeps its key for the retry", async () => {
    await p.goto(`${ORIGIN}/releases`)
    await p.waitForSelector("[data-client-floor]")
    await p.getByRole("button", { name: "Change minimum version" }).click()
    const d = dialog("Change the minimum CLI version")
    await d.getByLabel("Minimum version").fill("not-a-version")
    await d.getByLabel("Reference", { exact: true }).fill("TICKET-47")
    await d.getByLabel(/Type/).fill("update-policy")
    expect(await d.getByRole("button", { name: "Set minimum" }).isDisabled(), "a malformed version must block")
    await d.getByLabel("Minimum version").fill("2026.11.18.1")
    await d.getByLabel("Reason shown to users").fill("Security fix")
    await d.getByRole("button", { name: "Set minimum" }).click()
    await p.waitForFunction(() => document.querySelector("[data-client-floor]")?.textContent?.includes("2026.11.18.1"))
    expect((await text("[data-client-floor]")).includes("Minimum CLI version"), "the floor panel lost its title")
    expect(lastWrite().body.expected_revision === 2 && lastWrite().body.reason === "Security fix" && lastWrite().target === "client-update-policy", "floor write")
    // A second operator changed the policy meanwhile: this panel's revision is stale and the write is refused.
    await p.getByRole("button", { name: "Change minimum version" }).click()
    const stale = dialog("Change the minimum CLI version")
    await stale.getByLabel("Minimum version").fill("2026.11.19.1")
    await stale.getByLabel("Reason shown to users").fill("Another fix")
    await stale.getByLabel("Reference", { exact: true }).fill("TICKET-49")
    await stale.getByLabel(/Type/).fill("update-policy")
    world.floor.revision = 9
    await stale.getByRole("button", { name: "Set minimum" }).click()
    await stale.locator("[data-action-error]").waitFor()
    expect((await stale.locator("[data-action-error]").innerText()).includes("support_fixture_floor"), "stale revision reference missing")
    await stale.getByRole("button", { name: "Cancel" }).click()
    await p.getByRole("tab", { name: "Release authority" }).click()
    await p.waitForSelector("[data-release-authority]")
    await p.getByRole("button", { name: "Import a signed bundle" }).click()
    const b = dialog("Import a signed release bundle")
    await b.getByLabel(/Signed bundle/).fill(JSON.stringify({ release_id: "rel_1", signatures: [] }))
    await b.getByLabel("Reference", { exact: true }).fill("TICKET-48")
    await b.getByLabel(/Type/).fill("rel_1")
    await b.getByRole("button", { name: "Import bundle" }).click()
    await b.locator("[data-action-error]").waitFor()
    expect((await b.locator("[data-action-error]").innerText()).includes("support_fixture_bundle"), "reference missing on the refusal")
    const first = lastWrite().key
    await b.getByRole("button", { name: "Import bundle" }).click()
    await b.locator("[data-action-error]").waitFor()
    expect(writes.at(-2).key === first && lastWrite().key === first && lastWrite().target === "rel_1", "a retry must keep the idempotency key")
    // Edited input after a failure is a different action: it gets a new key.
    await b.getByLabel("Reference", { exact: true }).fill("TICKET-48b")
    const before = writes.length
    await b.getByRole("button", { name: "Import bundle" }).click()
    for (let i = 0; i < 50 && writes.length === before; i++) await new Promise((r) => setTimeout(r, 100))
    expect(writes.length === before + 1 && lastWrite().key !== first && lastWrite().reason === "TICKET-48b", "edited input must use a new idempotency key")
    await b.getByRole("button", { name: "Cancel" }).click()
    await p.getByRole("button", { name: "Request a release decision" }).click()
    const r = dialog("Request a release decision")
    await r.getByLabel("Release id").fill("rel_2")
    await r.getByLabel("Version").fill("2026.11.18.2")
    await r.getByLabel("Policy revision").fill("7")
    await submit(r, "Create request")
    await p.waitForSelector('[data-table="release requests"] >> text=rel_2')
    expect(lastWrite().body.action === "promote" && lastWrite().body.rollout_percentage === 100 && lastWrite().body.platform === "linux", "request write")
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
  await step("user: sign out everywhere needs the id typed and sends the account version; a grant gives a plan that can be revoked", async () => {
    await p.goto(`${ORIGIN}/users/usr_001`)
    await p.waitForSelector("[data-user-actions]")
    await p.getByRole("button", { name: "Sign out everywhere" }).click()
    const out = dialog("Sign person1@example.test out everywhere")
    await out.getByLabel("Reference", { exact: true }).fill("TICKET-60")
    expect(await out.getByRole("button", { name: "Sign out everywhere" }).isDisabled(), "sign out everywhere must need the typed id")
    await out.getByLabel(/Type/).fill("usr_001")
    await out.getByRole("button", { name: "Sign out everywhere" }).click()
    await p.waitForFunction(() => !document.querySelector('[data-action-dialog="Sign person1@example.test out everywhere"]'))
    const w = lastWrite()
    expect(w.path === "/v1/admin/accounts/usr_001/sessions/revoke" && w.target === "usr_001" && w.body.revision === 1, `sessions revoke ${JSON.stringify(w)}`)
    await p.waitForSelector("[data-plan-actions]")
    expect((await p.locator("[data-plan-actions]").getByRole("button", { name: "Revoke grant" }).count()) === 0, "no grant to revoke yet")
    await p.getByRole("button", { name: "Grant a plan" }).click()
    const g = dialog("Grant a plan to personal:usr_001")
    await g.getByLabel("Days").fill("45")
    await submit(g, "Grant plan")
    await p.waitForSelector("[data-plan-granted]")
    const grant = lastWrite()
    expect(grant.path === "/v1/admin/billing/workspaces/personal%3Ausr_001/grant" && grant.target === "personal:usr_001" && grant.body.revision === 1 && grant.body.product_code === "pro_monthly" && grant.body.days === 45 && !("quantity" in grant.body), `grant write ${JSON.stringify(grant)}`)
    await p.getByRole("button", { name: "Revoke grant" }).click()
    const r = dialog("Revoke the grant of personal:usr_001")
    await r.getByLabel("Reference", { exact: true }).fill("TICKET-61")
    await r.getByLabel(/Type/).fill("personal:usr_001")
    await r.getByRole("button", { name: "Revoke grant" }).click()
    await p.waitForFunction(() => !document.querySelector("[data-plan-granted]") && !document.querySelector('[data-action-dialog="Revoke the grant of personal:usr_001"]'))
    expect(lastWrite().path === "/v1/admin/billing/workspaces/personal%3Ausr_001/revoke" && lastWrite().body.revision === 2, "revoke write")
  })
  await step("team: the plan shows its seats, a team grant asks for seats, and a paid team has nothing to revoke", async () => {
    await p.goto(`${ORIGIN}/teams/team_a`)
    await p.waitForSelector("[data-plan-actions]")
    // The plan comes from its own query, which can land after the plan actions render.
    await p.waitForFunction(() => document.querySelector("main")?.textContent?.includes("team_monthly"))
    expect((await text("main")).includes("team_monthly"), "the team plan is not shown")
    expect((await p.locator("[data-plan-actions]").getByRole("button", { name: "Revoke grant" }).count()) === 0, "a Stripe team offers revoke")
    await p.getByRole("button", { name: "Grant a plan" }).click()
    const g = dialog("Grant a plan to team:team_a")
    expect((await g.getByLabel("Seats").count()) === 1, "a team grant needs seats")
    await g.getByRole("button", { name: "Cancel" }).click()
  })
  await step("entitlements: bandwidth is granted in GB with an end date, shows in the numbers, and is revoked; each write has its target, reason and key", async () => {
    await p.goto(`${ORIGIN}/users/usr_001`)
    await p.waitForSelector("[data-entitlements]")
    expect((await text("[data-entitlement-bandwidth]")).includes("1.2 GB of 5.0 GB"), `bandwidth before: ${await text("[data-entitlement-bandwidth]")}`)
    await p.getByRole("button", { name: "Grant bandwidth" }).click()
    const d = dialog("Grant bandwidth to personal:usr_001")
    await d.getByLabel("Amount (GB)").fill("250")
    const ends = new Date(Date.now() + 5 * 864e5).toISOString().slice(0, 16)
    await d.getByLabel("Ends").fill(ends)
    await submit(d, "Grant bandwidth")
    await p.waitForFunction(() => document.querySelector("[data-entitlement-bandwidth]")?.textContent?.includes("granted"))
    const w = lastWrite()
    expect(w.path === "/v1/admin/billing/workspaces/personal%3Ausr_001/entitlements" && w.target === "personal:usr_001" && w.body.entitlement === "bandwidth" && w.body.amount === 250e9 && /^\d{4}-\d\d-\d\dT/.test(w.body.ends_at) && !("revision" in w.body), `bandwidth write ${JSON.stringify(w)}`)
    expect((await text("[data-entitlement-bandwidth]")).includes("1.2 GB of 255 GB") && (await text("[data-entitlement-bandwidth]")).includes("250 GB granted"), "the numbers do not show the grant")
    expect((await p.locator("[data-grants] li").count()) === 1 && (await text("[data-grants]")).includes("Ends"), "the grant is not listed with its end")
    await p.locator("[data-grants]").getByRole("button", { name: "Revoke" }).click()
    const r = dialog("Revoke the Bandwidth grant")
    await submit(r, "Revoke grant")
    await p.waitForFunction(() => !document.querySelector("[data-entitlement-bandwidth]")?.textContent?.includes("granted"))
    expect(lastWrite().path.endsWith("/entitlements/revoke") && lastWrite().body.grant_id === "grant_1", "revoke write")
    await p.getByRole("button", { name: "Grant bandwidth" }).click()
    const bad = dialog("Grant bandwidth to personal:usr_001")
    await bad.getByLabel("Ends").fill("2001-01-01T00:00")
    await bad.getByLabel("Reference", { exact: true }).fill("TICKET-70")
    expect(await bad.getByRole("button", { name: "Grant bandwidth", exact: true }).isDisabled(), "a past end date can be submitted")
    await bad.getByRole("button", { name: "Cancel" }).click()
  })
  await step("entitlements: a limit is overridden for a team (seats are offered only there), shows as granted, and a newer override replaces it", async () => {
    await p.goto(`${ORIGIN}/teams/team_a`)
    await p.waitForSelector("[data-entitlements]")
    expect((await text('[data-entitlement-limit="seats"]')).includes("4 used of 3"), "team seats row")
    expect((await text('[data-entitlement-limit="machines"]')).includes("2 of no limit"), "a team limit without an override reads no limit")
    await p.getByRole("button", { name: "Override a limit" }).click()
    const d = dialog("Override a limit of team:team_a")
    expect((await d.getByLabel("Limit").locator("option").allInnerTexts()).includes("Seats"), "a team can override seats")
    await d.getByLabel("Limit").selectOption("machines")
    await d.getByLabel("Value").fill("500")
    await submit(d, "Set limit")
    await p.waitForFunction(() => document.querySelector('[data-entitlement-limit="machines"]')?.textContent?.includes("granted"))
    expect((await text('[data-entitlement-limit="machines"]')).includes("2 of 500") && (await text('[data-entitlement-limit="machines"]')).includes("plan 100"), "the override is not shown against the plan")
    const w = lastWrite()
    expect(w.target === "team:team_a" && w.body.entitlement === "machines" && w.body.amount === 500 && !("ends_at" in w.body), `override write ${JSON.stringify(w.body)}`)
    await p.getByRole("button", { name: "Override a limit" }).click()
    const again = dialog("Override a limit of team:team_a")
    await again.getByLabel("Limit").selectOption("machines")
    await again.getByLabel("Value").fill("800")
    await submit(again, "Set limit")
    await p.waitForFunction(() => document.querySelector('[data-entitlement-limit="machines"]')?.textContent?.includes("of 800"))
    await p.waitForFunction(() => [...document.querySelectorAll("[data-grants] li")].filter((li) => li.textContent.includes("Revoked")).length === 1)
    await p.goto(`${ORIGIN}/users/usr_001`)
    await p.waitForSelector("[data-entitlements]")
    await p.getByRole("button", { name: "Override a limit" }).click()
    expect(!(await dialog("Override a limit of personal:usr_001").getByLabel("Limit").locator("option").allInnerTexts()).includes("Seats"), "a person is offered seats")
    await dialog("Override a limit of personal:usr_001").getByRole("button", { name: "Cancel" }).click()
  })
  await step("ban: an account is banned with its typed id, its subscriptions choice and the addresses to block, shows Banned with the reference, and is unbanned", async () => {
    await p.goto(`${ORIGIN}/users/usr_001`)
    await p.waitForSelector("[data-recent-addresses]")
    expect((await p.locator("[data-recent-addresses] li").count()) === 2, "the recent addresses are not listed")
    await p.getByRole("button", { name: "Ban account" }).click()
    const d = dialog("Ban person1@example.test")
    await d.getByLabel("Paid subscriptions").selectOption("period_end")
    await d.getByRole("checkbox", { name: /203\.0\.113\.9/ }).check()
    await d.getByLabel("Also block a range").fill("192.0.2.0/24")
    await d.getByLabel("Reference", { exact: true }).fill("TICKET-61")
    expect(await d.getByRole("button", { name: "Ban account", exact: true }).isDisabled(), "a ban must need the typed id")
    await d.getByLabel(/Type/).fill("usr_001")
    await d.getByRole("button", { name: "Ban account", exact: true }).click()
    await p.waitForFunction(() => document.querySelector('[data-state-word="banned"]') && document.body.textContent.includes("TICKET-61"))
    const w = lastWrite()
    expect(w.path === "/v1/admin/accounts/usr_001/ban" && w.target === "usr_001" && w.reason === "TICKET-61" && w.body.revision === 1 && w.body.subscriptions === "period_end" && JSON.stringify(w.body.block_ips) === JSON.stringify(["203.0.113.9", "192.0.2.0/24"]), `ban write ${JSON.stringify(w)}`)
    await p.waitForFunction(() => /blocked/i.test(document.querySelector('[data-ip="203.0.113.9"]')?.textContent ?? ""))
    await p.waitForFunction(() => !document.querySelector("[data-action-dialog]"))
    await p.getByRole("button", { name: "Unban account" }).click()
    const u = dialog("Unban person1@example.test")
    await u.getByLabel("Reference", { exact: true }).fill("TICKET-62")
    await u.getByLabel(/Type/).fill("usr_001")
    await u.getByRole("button", { name: "Unban account", exact: true }).click()
    await p.waitForFunction(() => !document.querySelector('[data-state-word="banned"]'))
    expect(lastWrite().path === "/v1/admin/accounts/usr_001/unban" && lastWrite().body.revision === 2, `unban write ${JSON.stringify(lastWrite())}`)
  })
  await step("ban: lists show Banned with the reference and filter on it; a team is banned and unbanned the same way", async () => {
    await p.goto(`${ORIGIN}/users`)
    await waitReady("users")
    await p.click("[data-filter-button]")
    await p.getByRole("menuitem", { name: "Status" }).click()
    await p.getByRole("menuitemradio", { name: "Banned", exact: true }).click()
    await p.waitForFunction(() => document.querySelector('[data-filter-chip="Status"]') && document.querySelectorAll('[data-table="users"] [data-state-word="banned"]').length > 0)
    const rows = await text('[data-table="users"]')
    expect(rows.includes("ABUSE-9") && !rows.includes("person1@example.test"), `banned list ${rows.slice(0, 200)}`)
    await p.goto(`${ORIGIN}/teams/team_a`)
    await p.waitForSelector("[data-team-actions]")
    await p.getByRole("button", { name: "Ban team" }).click()
    const d = dialog("Ban team_a")
    expect((await d.getByLabel("Also block a range").count()) === 0, "a team ban offers networks")
    await d.getByLabel("Paid subscriptions").selectOption("immediate")
    await d.getByLabel("Reference", { exact: true }).fill("TICKET-63")
    await d.getByLabel(/Type/).fill("team_a")
    await d.getByRole("button", { name: "Ban team", exact: true }).click()
    await p.waitForFunction(() => document.querySelector('[data-state-word="banned"]') && document.body.textContent.includes("TICKET-63"))
    expect(lastWrite().path === "/v1/admin/teams/team_a/ban" && lastWrite().body.revision === 2 && lastWrite().body.subscriptions === "immediate" && !("block_ips" in lastWrite().body), `team ban ${JSON.stringify(lastWrite())}`)
    await p.goto(`${ORIGIN}/teams`)
    await waitReady("teams")
    expect((await text('[data-table="teams"]')).includes("TICKET-63"), "the banned team is not shown in the list")
    await p.goto(`${ORIGIN}/teams/team_a`)
    await p.waitForSelector("[data-team-actions]")
    await p.waitForFunction(() => !document.querySelector("[data-action-dialog]"))
    await p.getByRole("button", { name: "Unban team" }).click()
    const u = dialog("Unban team_a")
    await u.getByLabel("Reference", { exact: true }).fill("TICKET-64")
    await u.getByLabel(/Type/).fill("team_a")
    await u.getByRole("button", { name: "Unban team", exact: true }).click()
    await p.waitForFunction(() => !document.querySelector('[data-state-word="banned"]'))
  })
  await step("IP blocks: the list pages and filters, a network is blocked with an end date and lifted; each write has its target, reference and key", async () => {
    await p.goto(`${ORIGIN}/ip-blocks`)
    await waitReady("IP blocks")
    expect((await text('[data-table="IP blocks"]')).includes("198.51.100.0/24") && (await text('[data-table="IP blocks"]')).includes("192.0.2.7/32"), "the blocks are not listed")
    await p.click("[data-filter-button]")
    await p.getByRole("menuitem", { name: "State" }).click()
    await p.getByRole("menuitemradio", { name: "Running", exact: true }).click()
    await p.waitForFunction(() => !document.querySelector('[data-table="IP blocks"]')?.textContent?.includes("192.0.2.7/32") && document.querySelector('[data-filter-chip="State"]'))
    await p.getByRole("button", { name: "Remove State filter" }).click()
    await p.getByRole("button", { name: "Block a network" }).click()
    const d = dialog("Block a network")
    await d.getByLabel("Address or range").fill("2001:db8:44::/48")
    const ends = new Date(Date.now() + 3 * 864e5).toISOString().slice(0, 16)
    await d.getByLabel("Ends").fill(ends)
    await submit(d, "Block network")
    await p.waitForFunction(() => document.querySelector('[data-table="IP blocks"]')?.textContent?.includes("2001:db8:44::/48"))
    const w = lastWrite()
    expect(w.path === "/v1/admin/ip-blocks" && w.target === "ip-blocks" && w.reason === "TICKET-42" && w.body.cidr === "2001:db8:44::/48" && /^\d{4}-\d\d-\d\dT/.test(w.body.ends_at), `block write ${JSON.stringify(w)}`)
    const bad = await (async () => {
      await p.getByRole("button", { name: "Block a network" }).click()
      const b = dialog("Block a network")
      await b.getByLabel("Address or range").fill("not an address")
      await b.getByLabel("Reference", { exact: true }).fill("TICKET-65")
      const disabled = await b.getByRole("button", { name: "Block network", exact: true }).isDisabled()
      await b.getByRole("button", { name: "Cancel" }).click()
      return disabled
    })()
    expect(bad, "an invalid address can be submitted")
    await p.locator('[data-table="IP blocks"] tbody tr', { hasText: "2001:db8:44::/48" }).getByRole("button", { name: "Lift" }).click()
    const l = dialog(/Lift the block on 2001:db8:44::\/48/.source ? "Lift the block on 2001:db8:44::/48" : "")
    await submit(l, "Lift block")
    await p.waitForFunction(() => !document.querySelector('[data-action-dialog="Lift the block on 2001:db8:44::/48"]'))
    expect(lastWrite().path.endsWith("/remove") && lastWrite().target === lastWrite().path.split("/")[4], `lift write ${JSON.stringify(lastWrite())}`)
  })
  await step("announcements: listed with state and seen counts; a notice for one user is written with a live preview, edited and retracted; two live banners for one audience are refused; each write has its target, reference and key", async () => {
    await p.goto(`${ORIGIN}/announcements`)
    await waitReady("announcements")
    const table = '[data-table="announcements"]'
    expect((await text(table)).includes("Paperboat is in beta") && (await text(table)).includes("12 dismissed") && (await text(table)).includes("31 read"), "the announcements are not listed with their counts")
    await p.click("[data-filter-button]")
    await p.getByRole("menuitem", { name: "State" }).click()
    await p.getByRole("menuitemradio", { name: "Live", exact: true }).click()
    await p.waitForFunction(() => !document.querySelector('[data-table="announcements"]')?.textContent?.includes("Relays in Singapore") && document.querySelector('[data-filter-chip="State"]'))
    await p.getByRole("button", { name: "Remove State filter" }).click()
    await p.getByRole("button", { name: "New announcement" }).click()
    const d = dialog("New announcement")
    await d.getByLabel("Title").fill("Your plan was upgraded")
    await d.locator("#announcement-body").fill("Thanks for being early.")
    await d.getByLabel("Link").fill("/billing")
    expect((await d.locator('[data-preview="notice"]').innerText()).includes("Your plan was upgraded"), "the preview does not follow the title")
    await d.getByLabel("Who gets it").selectOption("user")
    await d.getByLabel("Search users").fill("person3")
    await d.getByRole("button", { name: /person3@example.test/ }).click()
    expect(await d.getByRole("button", { name: "Send", exact: true }).isDisabled(), "send is enabled before the audit reference is given")
    await p.setViewportSize({ width: 1280, height: 1500 })
    await p.screenshot({ path: `${shots}/announcement-dialog.png` })
    await p.setViewportSize({ width: 1280, height: 800 })
    await submit(d, "Send")
    await p.waitForFunction(() => document.querySelector('[data-table="announcements"]')?.textContent?.includes("Your plan was upgraded"))
    const w = lastWrite()
    expect(w.method === "POST" && w.path === "/v1/admin/announcements" && w.target === "announcements" && w.reason === "TICKET-42" && w.key.length >= 8 && w.body.kind === "notice" && w.body.audience_kind === "user" && w.body.audience_value === "usr_003" && w.body.link_url === "/billing", `create write ${JSON.stringify(w)}`)
    await p.locator(`${table} tbody tr`, { hasText: "Your plan was upgraded" }).locator("[data-row-menu]").click()
    await p.getByRole("menuitem", { name: "Edit…", exact: true }).click()
    const e = dialog("Edit announcement")
    expect(await e.getByLabel("Where").isDisabled(), "the kind can change on edit")
    await e.getByLabel("Title").fill("Your plan was upgraded to Pro")
    await submit(e, "Save")
    await p.waitForFunction(() => document.querySelector('[data-table="announcements"]')?.textContent?.includes("upgraded to Pro"))
    expect(lastWrite().method === "PUT" && lastWrite().target === lastWrite().path.split("/")[4] && lastWrite().body.title === "Your plan was upgraded to Pro", `edit write ${JSON.stringify(lastWrite())}`)
    await p.getByRole("button", { name: "New announcement" }).click()
    const b = dialog("New announcement")
    await b.getByLabel("Where").selectOption("banner")
    await b.getByLabel("Title").fill("A second ribbon")
    await p.setViewportSize({ width: 1280, height: 1500 })
    await p.screenshot({ path: `${shots}/announcement-banner-dialog.png` })
    await p.setViewportSize({ width: 1280, height: 800 })
    await submit(b, "Send")
    await b.getByText("already runs for this audience").waitFor()
    await b.getByRole("button", { name: "Cancel" }).click()
    await rowAction(`${table} tbody tr:has-text("upgraded to Pro")`, "Retract")
    await submit(dialog("Retract “Your plan was upgraded to Pro”"), "Retract")
    await p.waitForFunction(() => [...document.querySelectorAll('[data-table="announcements"] tbody tr')].some((r) => r.textContent.includes("upgraded to Pro") && r.textContent.includes("retracted")))
    expect(lastWrite().path.endsWith("/retract") && lastWrite().target === lastWrite().path.split("/")[4], `retract write ${JSON.stringify(lastWrite())}`)
    await p.setViewportSize({ width: 390, height: 844 })
    await p.screenshot({ path: `${shots}/announcements-phone.png` })
    await p.setViewportSize({ width: 1280, height: 800 })
  })
  await step("notices: a price increase is previewed by the server (refused under 30 days, then rendered with its recipient count), sent to a test user with target, reference and key, listed, and its audit entry appears", async () => {
    await p.goto(`${ORIGIN}/notices`)
    await waitReady("notices")
    expect((await text('[data-table="notices"]')).includes("No notices sent"), "the list is not empty before sending")
    await p.getByRole("button", { name: "New notice" }).click()
    const d = dialog("New customer notice")
    await d.getByLabel("Who gets it").selectOption("user")
    await d.getByLabel("Search users").fill("person3")
    await d.getByRole("button", { name: /person3@example.test/ }).click()
    const day = (n) => new Date(Date.now() + n * 86400e3).toISOString().slice(0, 10)
    await d.getByLabel("Takes effect on").fill(day(10))
    await d.getByLabel("What changes").fill("Pro goes from $9 to $12 a month.")
    await d.locator("[data-notice-preview]").getByText("at least 30 days").waitFor()
    await d.getByLabel("Reference", { exact: true }).fill("TICKET-42")
    expect(await d.getByRole("button", { name: "Send", exact: true }).isDisabled(), "send is enabled while the server refuses the notice")
    await d.getByLabel("Takes effect on").fill(day(31))
    await d.locator("[data-notice-preview]").getByText("1 recipient").waitFor()
    expect((await d.locator("[data-notice-preview]").innerText()).includes("prices change"), "the server's subject is not shown")
    const dry = lastWrite()
    expect(dry.body.dry_run === true && dry.target === "notices" && dry.reason === "preview" && dry.key.length >= 8, `preview write ${JSON.stringify(dry)}`)
    expect(await d.getByRole("button", { name: "Send to 1", exact: true }).isEnabled(), "send stays disabled after a good preview")
    await p.setViewportSize({ width: 1280, height: 1500 })
    await p.screenshot({ path: `${shots}/notice-dialog.png` })
    await p.setViewportSize({ width: 1280, height: 800 })
    await submit(d, "Send to 1")
    await p.waitForFunction(() => document.querySelector('[data-table="notices"]')?.textContent?.includes("prices change"))
    // The dialog's preview may refetch once more after the send (the dialog invalidates every query): the send is the last write that is not a dry run.
    const w = writes.findLast((x) => !x.body?.dry_run)
    expect(w.method === "POST" && w.path === "/v1/admin/notices" && w.target === "notices" && w.reason === "TICKET-42" && w.key.length >= 8 && !w.body.dry_run && w.body.kind === "price_increase" && w.body.audience_kind === "user" && w.body.audience_value === "usr_003" && w.body.effective === day(31), `send write ${JSON.stringify(w)}`)
    await p.goto(`${ORIGIN}/audit`)
    await waitReady("audit events")
    expect((await text('[data-table="audit events"]')).includes("operator.customer_notice_send"), "the send has no audit entry")
  })
  await step("operators: you are marked and have no buttons on your own row; inviting shows the setup token once and lists the invitation", async () => {
    await p.goto(`${ORIGIN}/operators`)
    await waitReady("operators")
    expect((await p.locator('[data-operator-row="agent@example.test"] [data-row-menu]').count()) === 1, "other operators need actions")
    expect((await p.locator('[data-operator-row="root@example.test"]').count()) === 0, "your own row has actions (the server refuses them)")
    expect((await text("main")).includes("You"), "you are not marked")
    await p.getByRole("button", { name: "Invite operator" }).click()
    const d = dialog("Invite an operator")
    await d.getByLabel("Email").fill("new@example.test")
    await d.getByLabel("Role").selectOption("billing")
    await submit(d, "Create invitation")
    await p.waitForSelector("[data-one-time-secrets]")
    expect((await text("[data-secret]")) === "setup-FIXTURE-token", "the setup token was not shown")
    const w = lastWrite()
    expect(w.path === "/v1/admin/operators" && w.target === "operators" && w.body.email === "new@example.test" && w.body.role === "billing", `invite write ${JSON.stringify(w)}`)
    await p.getByText("I saved it").click()
    await p.getByRole("button", { name: "Done" }).click()
    await p.waitForFunction(() => !document.querySelector("[data-one-time-secrets]") && document.querySelector('[data-state-word="invited"]'))
    expect(!(await p.content()).includes("setup-FIXTURE-token"), "the setup token stayed on the page")
    await p.screenshot({ path: `${shots}/operators-light.png`, fullPage: true })
  })
  await step("operators: disable and MFA reset need the email typed; enable undoes the disable; sessions can be ended one by one", async () => {
    const opMenu = async (item) => {
      await p.locator('[data-operator-row="agent@example.test"] [data-row-menu]').click()
      await p.getByRole("menuitem", { name: item }).click()
    }
    await opMenu("Disable…")
    const dis = dialog("Disable agent@example.test")
    await dis.getByLabel("Reference", { exact: true }).fill("TICKET-50")
    expect(await dis.getByRole("button", { name: "Disable operator" }).isDisabled(), "disable must need the typed email")
    await dis.getByLabel(/Type/).fill("agent@example.test")
    await dis.getByRole("button", { name: "Disable operator" }).click()
    await p.waitForSelector('tr:has([data-operator-row="agent@example.test"]) [data-state-word="disabled"]')
    expect(lastWrite().path === "/v1/admin/operators/operator_agent/disable" && lastWrite().target === "operator_agent", "disable write")
    await opMenu("Enable…")
    await submit(dialog("Enable agent@example.test"), "Enable operator")
    await p.waitForSelector('tr:has([data-operator-row="agent@example.test"]) [data-state-word="active"]')
    expect(lastWrite().path === "/v1/admin/operators/operator_agent/enable", "enable write")
    await opMenu("Reset MFA…")
    const reset = dialog("Reset the authenticator of agent@example.test")
    await reset.getByLabel("Reference", { exact: true }).fill("TICKET-51")
    expect(await reset.getByRole("button", { name: "Reset authenticator" }).isDisabled(), "reset must need the typed email")
    await reset.getByLabel(/Type/).fill("agent@example.test")
    await reset.getByRole("button", { name: "Reset authenticator" }).click()
    await p.waitForFunction(() => !document.querySelector('[data-action-dialog="Reset the authenticator of agent@example.test"]'))
    expect(lastWrite().path === "/v1/admin/operators/operator_agent/mfa-reset" && lastWrite().target === "operator_agent", "reset write")
    await p.waitForSelector('tr:has([data-operator-row="agent@example.test"]) [data-live-sessions="0"]')
  })
  await step("operators: a single live session is chosen from the list and ended", async () => {
    // The fixture's reset above ended the live count; give the operator two sessions again.
    world.operators.find((o) => o.id === "operator_agent").live_sessions = 2
    world.operatorSessions.operator_agent = ["a", "b"].map((c) => fixtureSession(c))
    await p.reload()
    await waitReady("operators")
    await p.locator('[data-operator-row="agent@example.test"] [data-row-menu]').click()
    await p.getByRole("menuitem", { name: "End sessions…" }).click()
    const d = dialog("Sessions of agent@example.test")
    await p.waitForSelector("[data-operator-sessions]")
    expect((await d.getByLabel("Session", { exact: true }).locator("option").count()) === 3, "all + two sessions expected")
    await d.getByLabel("Session", { exact: true }).selectOption("b".repeat(64))
    await submit(d, "End session")
    await p.waitForSelector('tr:has([data-operator-row="agent@example.test"]) [data-live-sessions="1"]')
    const w = lastWrite()
    expect(w.path === "/v1/admin/operators/operator_agent/sessions/revoke" && w.body.session_id === "b".repeat(64), `revoke write ${JSON.stringify(w)}`)
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
    await p.click("[data-operator-email]")
    await p.getByRole("menuitem", { name: "Sign out" }).click()
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
    expect(!nav.some((t) => t.includes("Audit log") || t.includes("Operators")) && nav.some((t) => t.includes("Users")), `nav was ${nav.join("|")}`)
    await p.goto(`${ORIGIN}/audit`)
    await p.waitForSelector("[data-error-block]")
    expect((await text("[data-error-block]")).includes("req_forbidden"), "403 reference missing")
    await p.screenshot({ path: `${shots}/forbidden-light.png`, fullPage: true })
  })
  await step("a support-role operator is refused the operators list and its sessions with a reference", async () => {
    await p.goto(`${ORIGIN}/operators`)
    await p.waitForSelector("[data-error-block]")
    expect((await text("[data-error-block]")).includes("req_forbidden"), "403 reference missing")
    const sessions = await p.evaluate(async () => (await fetch("/api/v1/admin/operators/operator_agent/sessions")).status)
    expect(sessions === 403, `a support operator read operator sessions: ${sessions}`)
  })
  await step("a support-role operator sees no write buttons", async () => {
    await p.goto(`${ORIGIN}/users/usr_001`)
    await p.waitForSelector('[data-table="machines"][data-state="ready"]')
    expect((await p.getByRole("button", { name: /Ban account|Unban account|Rotate SSH user CA|Sign out everywhere|Grant a plan|Grant bandwidth|Override a limit/ }).count()) === 0, "support sees account writes")
    await p.goto(`${ORIGIN}/billing`)
    await p.getByRole("tab", { name: "Plans" }).click()
    await waitReady("plans and prices")
    expect((await p.getByRole("button", { name: /Add product|Edit/ }).count()) === 0, "support sees product writes")
    const refused = await p.evaluate(async () => (await fetch("/api/v1/admin/regions/x/disable", { method: "POST", headers: { "content-type": "application/json", "operator-target": "x", "operator-reason": "ticket-1", "idempotency-key": "key-12345678" }, body: "{}" })).status)
    expect(refused === 403, `a support write answered ${refused}`)
  })
  await step("an invited operator is asked for the setup token and a password, a wrong token names its reference, and setup leads to authenticator enrolment", async () => {
    await p.click("[data-operator-email]")
    await p.getByRole("menuitem", { name: "Sign out" }).click()
    await p.waitForURL(/\/login/)
    // The email answer never says who has an invitation: the invitee asks for the token step themselves.
    await p.getByRole("button", { name: "I have a setup token" }).click()
    await p.waitForSelector('[data-login-step="setup_invited"]')
    await p.fill("#email", "new@example.test")
    await p.fill("#token", "wrong-token")
    await p.fill("#password", "a sufficiently long password")
    await p.click('button[type="submit"]')
    await p.waitForSelector("[data-login-error]")
    expect((await text("[data-login-error]")).includes("req_setup"), "no reference on a wrong token")
    await p.fill("#token", "setup-FIXTURE-token")
    await p.click('button[type="submit"]')
    await p.waitForSelector("[data-mfa-enroll]")
    expect(seen.setup.token === "setup-FIXTURE-token" && seen.setup.email === "new@example.test", "setup body")
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
