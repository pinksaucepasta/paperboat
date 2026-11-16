import { afterEach, beforeAll, beforeEach, expect, test } from "bun:test"
import { createHash } from "node:crypto"
import { sampleFor } from "@/lib/api-sample"
import { lockVault, vaultUnlocked } from "@/lib/envvault/crypto"
import { createTeamVault, resetVault, rotatePersonalKey } from "@/lib/envvault/manage"
import { fixture, loadVaultWasm, WasmWorker } from "@/lib/envvault/testkit"
import { unlockVault, type Vault } from "@/lib/envvault/vault"

// Team vault creation, personal key rotation and vault reset on the real vault WASM, against a vault the CLI sealed.
// The server is a stand-in that stores the vault a request carries, as the control plane does.

beforeAll(loadVaultWasm)

const realFetch = globalThis.fetch
const realWorker = (globalThis as { Worker?: unknown }).Worker
let stored: Vault
let calls: string[]
const account = fixture.vault.account_id

const keep = (envelope: string) => {
  stored = {
    issuer: fixture.vault.issuer,
    account_id: account,
    generation: stored.generation + 1,
    document_id: `sha256:${createHash("sha256").update(Buffer.from(envelope, "base64url")).digest("hex")}`,
    envelope,
  }
}

beforeEach(() => {
  ;(globalThis as { Worker: unknown }).Worker = WasmWorker
  ;(globalThis as { addEventListener?: unknown }).addEventListener ??= () => {}
  stored = structuredClone(fixture.vault)
  calls = []
  globalThis.fetch = (async (input: string | URL, init?: RequestInit) => {
    const url = new URL(String(input), "http://console.example.test")
    const method = init?.method ?? "GET"
    calls.push(`${method} ${url.pathname}`)
    const body = typeof init?.body === "string" ? JSON.parse(init.body) : undefined
    const ok = (data: unknown) => Response.json({ data })
    if (url.pathname === "/api/v1/me") return ok(sampleFor("GET /v1/me", { id: account }))
    if (url.pathname === "/api/v1/environment-vault") return ok(stored)
    if (url.pathname === "/api/v1/environment/scopes") return ok({ rotation_required: false, key_epoch: 1, scopes: [] })
    const team = {
      ...sampleFor("GET /v1/environment/teams/{team_id}"),
      team_id: "acme",
      owner_account: account,
      generation: 1,
      key_epoch: 0,
      members: [
        { account_id: account, membership_generation: 1, role: "owner", active: true, grant_epoch: 0, env_permission: "write" },
      ],
      rotation_required: false,
      entitled: true,
      record_sequence: 0,
    }
    if (url.pathname === "/api/v1/environment/teams/acme" && method === "GET") return ok(team)
    if (method === "POST" && /\/(reset|rotate-personal|teams)$/.test(url.pathname)) {
      keep(body.vault_envelope ?? body.envelope)
      return ok(url.pathname.endsWith("/teams") ? team : stored)
    }
    return new Response(null, { status: 404 })
  }) as typeof fetch
})

afterEach(() => {
  lockVault()
  globalThis.fetch = realFetch
  ;(globalThis as { Worker?: unknown }).Worker = realWorker
})

const opens = (password: string) =>
  unlockVault(password).then(
    () => {
      lockVault()
      return true
    },
    () => false,
  )

test("a vault with a lost password is reset to a new one without unlocking it", async () => {
  await resetVault("brand new password")
  expect(vaultUnlocked()).toBe(true)
  expect(calls).toContain("POST /api/v1/environment-vault/reset")
  lockVault()
  expect(await opens(fixture.password)).toBe(false)
  expect(await opens("brand new password")).toBe(true)
})

test("rotating the personal key publishes one new vault version and keeps the password", async () => {
  await unlockVault(fixture.password)
  await rotatePersonalKey()
  expect(vaultUnlocked()).toBe(true)
  expect(stored.generation).toBe(fixture.vault.generation + 1)
  expect(calls).toContain("POST /api/v1/environment/rotate-personal")
  lockVault()
  expect(await opens(fixture.password)).toBe(true)
})

test("a team's vault is created and its key lands in the owner's vault", async () => {
  await unlockVault(fixture.password)
  await createTeamVault("acme")
  expect(calls).toContain("POST /api/v1/environment/teams")
  expect(stored.generation).toBe(fixture.vault.generation + 1)
  expect(vaultUnlocked()).toBe(true)
})
