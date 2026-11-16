import { afterEach, beforeEach, expect, test } from "bun:test"
import { ApiError } from "@/lib/api"
import { lockVault, vaultCrypto } from "@/lib/envvault/crypto"
import { createNamedVault, missingVault, unknownVault, VaultLockedError, withVaultKeys, withWraps, wrapVault } from "@/lib/recipe-vaults"
import { sampleFor } from "@/lib/api-sample"

const missing = (vault: string) => new ApiError(422, "vault_key_missing", "x", { vault })

test("a vault the recipe newly reads is wrapped and the write is sent again with it", async () => {
  const sent: string[][] = []
  const result = await withWraps(
    async (wraps) => {
      sent.push(wraps.map((w) => w.vault))
      const next = ["a", "b"].find((v) => !wraps.some((w) => w.vault === v))
      if (next) throw missing(next)
      return "saved"
    },
    async (vault) => ({ vault, wrapped: `wrapped-${vault}` }),
  )
  expect(result).toBe("saved")
  expect(sent).toEqual([[], ["a"], ["a", "b"]])
})

test("other refusals, a vault wrapped twice and too many vaults stop the loop", async () => {
  const refused = new ApiError(422, "invalid_recipe", "bad file")
  expect(withWraps(() => Promise.reject(refused), async (vault) => ({ vault, wrapped: "w" }))).rejects.toBe(refused)
  // The server asking again for a vault already wrapped is its refusal, not a loop.
  expect(withWraps(() => Promise.reject(missing("a")), async (vault) => ({ vault, wrapped: "w" }))).rejects.toMatchObject({ code: "vault_key_missing" })
  let n = 0
  expect(withWraps(() => Promise.reject(missing(`v${n++}`)), async (vault) => ({ vault, wrapped: "w" }))).rejects.toMatchObject({ code: "vault_key_missing" })
})

test("the server's refusals name the vault", () => {
  expect(missingVault(missing("a"))).toBe("a")
  expect(missingVault(new ApiError(422, "invalid_recipe", "x"))).toBeUndefined()
  expect(unknownVault(new ApiError(422, "unknown_vault", "x", { name: "ci" }))).toBe("ci")
})

// The vault engine is a worker; a stand-in answers its actions so the calls the console makes can be read.
const realFetch = globalThis.fetch
const realWorker = (globalThis as { Worker?: unknown }).Worker
let requests: Record<string, unknown>[]
let fetched: { url: string; method: string; body?: unknown }[]
class FakeWorker {
  onmessage?: (event: { data: { id: number; reply: string } }) => void
  onerror?: () => void
  postMessage({ id, request }: { id: number; request: string }) {
    const body = JSON.parse(request) as Record<string, unknown>
    requests.push(body)
    const data =
      body.action === "named-vault-recipe-wrap"
        ? { vault: body.name, wrapped: "d3JhcHBlZA==" }
        : body.action === "named-vault-wraps"
          ? { wraps: [{ recipient_kind: "account", recipient_id: "a1", wrapped: "bmV3" }] }
          : {}
    queueMicrotask(() => this.onmessage?.({ data: { id, reply: JSON.stringify({ data }) } }))
  }
  terminate() {}
}
beforeEach(() => {
  requests = []
  fetched = []
  ;(globalThis as { Worker: unknown }).Worker = FakeWorker
  ;(globalThis as { addEventListener?: unknown }).addEventListener ??= () => {}
  globalThis.fetch = (async (input: string | URL, init?: RequestInit) => {
    const url = new URL(String(input), "http://console.example.test")
    fetched.push({ url: url.pathname, method: init?.method ?? "GET", body: init?.body ? JSON.parse(String(init.body)) : undefined })
    const vault = { ...sampleFor("GET /v1/workspaces/{ws}/env-vaults/{name}").vault as object, name: "canary-env", wraps: [{ recipient_kind: "account", recipient_id: "a1", wrapped: "b3Blbg==" }] }
    return Response.json({ data: { vault } })
  }) as typeof fetch
})
afterEach(() => {
  lockVault()
  globalThis.fetch = realFetch
  ;(globalThis as { Worker?: unknown }).Worker = realWorker
})

const token = { id: "abcdefghij", publicKey: "wkZEQafgKPkDFV+zmfJURuecdF9XYLcEKv9BGKyHe1c=" }

test("wrapping needs the ENV vault unlocked and says so when it is not", async () => {
  expect(wrapVault("personal", "canary-env", token)).rejects.toBeInstanceOf(VaultLockedError)
  expect(createNamedVault("personal", "canary-env")).rejects.toBeInstanceOf(VaultLockedError)
  expect(fetched).toEqual([])
})

test("a vault's key is read, opened by the engine and wrapped to the recipe's public key", async () => {
  await vaultCrypto({ action: "unlock" })
  const wrapped = await wrapVault("personal", "canary-env", token)
  expect(wrapped).toEqual({ vault: "canary-env", wrapped: "d3JhcHBlZA==" })
  expect(fetched[0]).toMatchObject({ url: "/api/v1/workspaces/personal/env-vaults/canary-env", method: "GET" })
  expect(requests.at(-1)).toEqual({
    action: "named-vault-recipe-wrap",
    name: "canary-env",
    vault_wraps: [{ recipient_kind: "account", recipient_id: "a1", wrapped: "b3Blbg==" }],
    recipe_public_key: token.publicKey,
    token_id: token.id,
  })
})

test("a recipe write that reads a vault is sent with the engine's wrap", async () => {
  await vaultCrypto({ action: "unlock" })
  const bodies: unknown[] = []
  await withVaultKeys("personal", token, async (wraps) => {
    bodies.push([...wraps])
    if (!wraps.length) throw missing("canary-env")
  })
  expect(bodies).toEqual([[], [{ vault: "canary-env", wrapped: "d3JhcHBlZA==" }]])
})

test("a new vault's key is made by the engine (for the team too) and sent as its wraps", async () => {
  await vaultCrypto({ action: "unlock" })
  await createNamedVault("acme", "canary-env", "acme")
  expect(requests.at(-1)).toEqual({ action: "named-vault-wraps", name: "canary-env", team_id: "acme" })
  expect(fetched.at(-1)).toMatchObject({
    url: "/api/v1/workspaces/acme/env-vaults",
    method: "POST",
    body: { name: "canary-env", wraps: [{ recipient_kind: "account", recipient_id: "a1", wrapped: "bmV3" }] },
  })
})
