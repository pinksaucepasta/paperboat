// Tiny mock control plane for the flow-state check (scripts/check-flow-state.mjs).
// It answers what the console's pages need (shapes generated from
// ../lib/api-responses.generated.json, so the console's response validation passes),
// and treats any request carrying a session cookie as signed in: no WorkOS, no prod server.
//
// Run (from console/):
//   node scripts/flow-state-mock.mjs            # listens on 127.0.0.1:4101 (MOCK_PORT to change)
//   PAPERBOAT_SERVER_URL=http://127.0.0.1:4101 WORKOS_REDIRECT_URI=http://127.0.0.1:3101/auth/callback \
//     bun run dev -- --port 3101               # or: nice -n 15 bunx next build && nice -n 15 bunx next start --port 3101
//   CONSOLE_ORIGIN=http://127.0.0.1:3101 PW_MODULE=/tmp/opencode/flowstate/node_modules/playwright-core/index.mjs \
//     CHROME=$HOME/.cache/ms-playwright/chromium-1248/chrome-linux-arm64/chrome \
//     node scripts/check-flow-state.mjs
import http from "node:http"
import { readFileSync } from "node:fs"

const port = Number(process.env.MOCK_PORT ?? 4101)
const generated = JSON.parse(readFileSync(new URL("../lib/api-responses.generated.json", import.meta.url)))
const defs = generated.defs
const ops = Object.entries(generated.ops).map(([key, schema]) => {
  const [method, path] = key.split(" ")
  return { key, method, segments: path.split("/"), schema }
})

const now = () => new Date().toISOString()

// Minimal JSON that fits a schema (mirrors the console's lib/api-validate.ts).
function synth(schema, key = "", depth = 0) {
  if (!schema || depth > 9) return undefined
  // An unconstrained schema ({}) accepts anything but must still be present when required.
  if (Object.keys(schema).length === 0) return key ? `${key}-mock` : "mock"
  if (schema.$ref) return synth(defs[schema.$ref], key, depth + 1)
  if (schema.allOf) return Object.assign({}, ...schema.allOf.map((s) => synth(s, key, depth + 1) ?? {}))
  for (const choice of [...(schema.oneOf ?? []), ...(schema.anyOf ?? [])]) {
    if (choice?.type === "null" || choice?.$ref === undefined && choice?.type === undefined) continue
    const v = synth(choice, key, depth + 1)
    if (v !== undefined) return v
  }
  const types = schema.type === undefined ? [] : Array.isArray(schema.type) ? schema.type : [schema.type]
  const t = types.find((x) => x !== "null") ?? types[0]
  if (!t) {
    if (schema.properties) t = "object"
    else return undefined
  }
  if (t === "array") return []
  if (t === "object") {
    const o = {}
    for (const [k, s] of Object.entries(schema.properties ?? {})) {
      const v = synth(s, k, depth + 1)
      if (v !== undefined) o[k] = v
    }
    return o
  }
  if (t === "integer" || t === "number") return 1
  if (t === "boolean") return false
  // A page cursor the UI follows while truthy: empty ends the list on its first page.
  if (/cursor$/i.test(key)) return ""
  // Date-shaped strings: the UI parses these (RangeError kills the page on garbage).
  if (/(^|_)at$|date|time|start|end|until|period|expir|seen|joined|created|updated|renew|grace|birth|due/i.test(key)) return now()
  if (/email/i.test(key)) return "flow@example.test"
  if (/url|endpoint|address/i.test(key)) return "https://example.test/mock"
  if (/command/i.test(key)) return "curl -fsSL https://example.test/install.sh | sh"
  if (/token|secret|key/i.test(key)) return "mock-secret-token"
  if (/domain|hostname/i.test(key)) return "example.test"
  if (/platform/i.test(key)) return "linux"
  if (/arch/i.test(key)) return "arm64"
  return key ? `${key}-mock` : "mock"
}

function opFor(method, path) {
  const segs = `/v1/${path}`.split("/")
  let best
  for (const op of ops) {
    if (op.method !== method || op.segments.length !== segs.length) continue
    let lit = 0
    const fits = op.segments.every((s, i) => {
      if (s.startsWith("{")) return segs[i] !== ""
      lit++
      return s === segs[i]
    })
    if (fits && (!best || lit > best.lit)) best = { ...op, lit }
  }
  return best
}

const ME = { id: "acct-flow", email: "flow@example.test", display_name: "Flow", status: "active", role: "owner", workos_subject: "sub-flow" }
const WS = [
  { id: "personal", name: "Personal", kind: "personal", role: "owner" },
  { id: "t-flow", name: "Flow Team", kind: "team", role: "owner" },
]
const cap = { configured: true, observed: true }
const machine = (id, alias, platform) =>
  Object.assign(synth({ $ref: "Machine" }), {
    ownership: "personal", id, owner_account: "acct-flow", environment_id: "env-flow", alias, platform,
    architecture: "arm64", workspace_root: "/home/flow", state: "online", seat_state: "active",
    public_identity_key: "mock-key", can_manage: true, shared: false, online: true,
    network_approved: true, permissions: ["terminal", "files", "ssh", "preview", "config", "access"],
    installation_generation: 1, runtime_versions: {}, capabilities: { terminal_host: cap, session_host: cap, file_receive: cap, preview_launch: cap, keep_awake: cap, environment_injection: cap },
    availability: synth({ $ref: "AvailabilityPolicy" }), runtime_diagnostics: synth({ $ref: "UserMachineRuntimeDiagnostics" }),
    machine_capabilities: { schema: 1, desired: { terminal: true, managed_ssh: true, file_receive: true, preview_tunnel: true }, desired_version: 1, applied: { terminal: true, managed_ssh: true, file_receive: true, preview_tunnel: true }, applied_version: 1, status: "applied" },
    tags: [],
  })
const MACHINES = [machine("m-alpha", "flow-alpha", "linux"), machine("m-beta", "flow-beta", "darwin")]
let enrollment = null

const server = http.createServer((req, res) => {
  let body = ""
  req.on("data", (c) => (body += c))
  req.on("end", () => {
    const url = new URL(req.url, "http://mock")
    const path = url.pathname.replace(/^\/v1\//, "").replace(/\/$/, "")
    const method = req.method
    const cookie = req.headers.cookie ?? ""
    const send = (status, payload, headers = {}) => {
      res.writeHead(status, { "content-type": "application/json", "cache-control": "no-store", ...headers })
      res.end(JSON.stringify(payload))
    }
    if (!/pb-dev-session|__Host-pb-session/.test(cookie)) return send(401, { error: { code: "unauthenticated", message: "Authentication is required." } })
    if (method === "GET" && path === "auth/csrf") return send(200, { data: { csrf_token: "mock-csrf" } }, { "set-cookie": "pb-dev-csrf=mock-csrf; Path=/; HttpOnly" })
    if (method === "GET" && path === "me") return send(200, { data: ME })
    if (method === "GET" && path === "workspaces") return send(200, { data: { items: WS } })
    if (method === "GET" && path === "client-configuration") return send(200, { data: { installer_base_url: "https://example.test", local_access_base_domain: "example.test" } })
    if (method === "GET" && path === "machines/overview") return send(200, { data: { machine_count: 2, machine_limit: 100, available_machines: 2, bootstrap_command: "curl -fsSL https://example.test/install.sh | sh" } })
    if (method === "GET" && path === "machines/summary") return send(200, { data: synth(opFor("GET", path).schema) })
    // Single-segment reads must precede the :id match below.
    if (method === "GET" && (path === "machines/running-counts" || path === "machines/update-summary")) {
      const op = opFor(method, path)
      const data = op?.schema ? synth(op.schema) : {}
      if (data && typeof data === "object" && !Array.isArray(data)) for (const k of Object.keys(data)) if (Array.isArray(data[k])) data[k] = []
      return send(200, { data: data ?? {} })
    }
    if (method === "GET" && path === "machines" && !path.includes("/")) return send(200, { data: { items: MACHINES, pagination: synth({ $ref: "InventoryOffsetPagination" }), total: 2 } })
    if (method === "POST" && path === "machine-enrollments") {
      enrollment = Object.assign(synth({ $ref: "MachineEnrollmentStart" }), { id: "enr-flow", operation_id: "op-flow", state: "awaiting_bootstrap", generation: 1, expires_at: now(), created_at: now(), updated_at: now(), bootstrap_token: "mock-secret-token", bootstrap_command: "curl -fsSL https://example.test/install.sh | PB_TOKEN='mock-secret-token' sh", server_url: "https://example.test" })
      return send(200, { data: enrollment })
    }
    let m
    if ((m = path.match(/^machines\/([^/]+)$/)) && method === "GET") {
      const found = MACHINES.find((x) => x.id === m[1])
      return found ? send(200, { data: found }) : send(404, { error: { code: "not_found", message: "Not found." } })
    }
    if ((m = path.match(/^machine-enrollments\/([^/]+)$/)) && method === "GET") {
      return enrollment ? send(200, { data: enrollment }) : send(404, { error: { code: "not_found", message: "Not found." } })
    }
    if ((m = path.match(/^machine-enrollments\/([^/]+)\/cancel$/)) && method === "POST") {
      if (enrollment) enrollment = { ...enrollment, state: "cancelled" }
      return send(200, { data: enrollment ?? {} })
    }
    if (method === "POST" && path === "teams") return send(200, { data: { url: "https://example.test/checkout" } })
    if (method === "GET" && path === "teams") return send(200, { data: [] })
    if (method === "GET" && path === "domains") return send(200, { data: { items: [] } })
    if ((m = path.match(/^workspaces\/[^/]+\/tags$/)) && method === "GET")
      return send(200, { data: Object.assign(synth({ $ref: "WorkspaceTags" }), { tags: [], can_edit: "all", policy_version: 1 }) })
    if ((m = path.match(/^workspaces\/[^/]+\/policy$/)) && method === "GET")
      return send(200, { data: Object.assign(synth({ $ref: "PolicyView" }), { workspace: "personal", version: 1, policy: "version: 1\n", digest: "mock", updated_at: now(), updated_by: "acct-flow", can_edit: "all" }) })
    if (method === "GET" && (m = path.match(/^teams\/([^/]+)\/members$/))) {
      return send(200, { data: { items: [{ account_id: "acct-flow", membership_generation: 1, role: "owner", active: true, email: "flow@example.test", display_name: "Flow", joined_at: now(), directory_managed: false, sso_exempt: false }], next_cursor: "", total: 1 } })
    }
    if (method === "GET" && (m = path.match(/^teams\/([^/]+)$/))) {
      const id = decodeURIComponent(m[1])
      const team = Object.assign(synth({ $ref: "TeamTeam" }), { team_id: id, owner_account: "acct-flow", display_name: "Flow Team", suspended: false })
      team.members = [{ account_id: "acct-flow", membership_generation: 1, role: "owner", active: true, email: "flow@example.test", display_name: "Flow", joined_at: now(), directory_managed: false, sso_exempt: false }]
      return send(200, { data: team })
    }
    // Generic: answer from the documented schema (arrays empty), so validation passes.
    const op = opFor(method, path.split("?")[0])
    if (op?.schema) {
      const data = synth(op.schema)
      if (data && typeof data === "object" && !Array.isArray(data)) for (const k of Object.keys(data)) if (Array.isArray(data[k])) data[k] = []
      return send(200, { data: data ?? {} })
    }
    if (method === "GET") return send(200, { data: { items: [] } })
    return send(200, { data: {} })
  })
})

server.listen(port, "127.0.0.1", () => console.log(`flow-state mock on 127.0.0.1:${port}`))
