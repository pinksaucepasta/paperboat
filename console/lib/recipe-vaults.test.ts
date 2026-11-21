import { afterEach, beforeEach, expect, test } from "bun:test"
import { ApiError } from "@/lib/api"
import { lockVault, vaultCrypto } from "@/lib/envvault/crypto"
import {
  createNamedVault,
  deleteNamedVault,
  listNamedVaults,
  missingVault,
  namedVaultNames,
  unknownVault,
  VaultLockedError,
  withVaultKeys,
  withWraps,
  wrapVault,
} from "@/lib/recipe-vaults"
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
  expect(
    withWraps(
      () => Promise.reject(refused),
      async (vault) => ({ vault, wrapped: "w" }),
    ),
  ).rejects.toBe(refused)
  // The server asking again for a vault already wrapped is its refusal, not a loop.
  expect(
    withWraps(
      () => Promise.reject(missing("a")),
      async (vault) => ({ vault, wrapped: "w" }),
    ),
  ).rejects.toMatchObject({ code: "vault_key_missing" })
  let n = 0
  expect(
    withWraps(
      () => Promise.reject(missing(`v${n++}`)),
      async (vault) => ({ vault, wrapped: "w" }),
    ),
  ).rejects.toMatchObject({ code: "vault_key_missing" })
})

test("the server's refusals name the vault", () => {
  expect(missingVault(missing("a"))).toBe("a")
  expect(missingVault(new ApiError(422, "invalid_recipe", "x"))).toBeUndefined()
  expect(unknownVault(new ApiError(422, "unknown_vault", "x", { name: "ci" }))).toBe("ci")
})

// The vault engine is a worker; a stand-in answers its actions so the calls the console makes can be read.
const accountVault = sampleFor("GET /v1/environment-vault")
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
          : body.action === "named-vault-values"
            ? { names: ["API_KEY"], ...(body.variable ? { envelope: "c2VhbGVk" } : {}) }
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
    // The account's password vault, which every engine call carries.
    if (url.pathname === "/api/v1/environment-vault") return Response.json({ data: accountVault })
    fetched.push({
      url: url.pathname,
      method: init?.method ?? "GET",
      body: init?.body ? JSON.parse(String(init.body)) : undefined,
    })
    const vault = {
      ...(sampleFor("GET /v1/workspaces/{ws}/env-vaults/{name}").vault as object),
      name: "canary-env",
      wraps: [{ recipient_kind: "account", recipient_id: "a1", wrapped: "b3Blbg==" }],
    }
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
    vault: accountVault,
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
  expect(requests.at(-1)).toEqual({
    action: "named-vault-wraps",
    vault: accountVault,
    name: "canary-env",
    team_id: "acme",
  })
  expect(fetched.at(-1)).toMatchObject({
    url: "/api/v1/workspaces/acme/env-vaults",
    method: "POST",
    body: { name: "canary-env", wraps: [{ recipient_kind: "account", recipient_id: "a1", wrapped: "bmV3" }] },
  })
})

test("a vault's variable names are read by the engine and no value is sent to the server", async () => {
  expect(namedVaultNames("personal", "canary-env")).rejects.toBeInstanceOf(VaultLockedError)
  await vaultCrypto({ action: "unlock" })
  expect(await namedVaultNames("personal", "canary-env")).toEqual(["API_KEY"])
  expect(requests.at(-1)).toMatchObject({
    action: "named-vault-values",
    name: "canary-env",
    vault_wraps: [{ recipient_id: "a1" }],
  })
  expect(requests.at(-1)).not.toHaveProperty("variable")
  expect(fetched.map((f) => f.method)).toEqual(["GET"])
})

test("setting or removing a variable sends the engine's sealed values at the revision it read", async () => {
  await vaultCrypto({ action: "unlock" })
  await namedVaultNames("personal", "canary-env", { variable: "API_KEY", value: "s3cret" })
  expect(requests.at(-1)).toMatchObject({
    action: "named-vault-values",
    variable: "API_KEY",
    value: "s3cret",
    delete: false,
  })
  const put = fetched.at(-1)!
  expect(put).toMatchObject({ url: "/api/v1/workspaces/personal/env-vaults/canary-env/values", method: "PUT" })
  expect(put.body).toEqual({
    expected_revision: expect.any(Number),
    key_epoch: expect.any(Number),
    envelope: "c2VhbGVk",
  })
  expect(JSON.stringify(put.body)).not.toContain("s3cret")
  await namedVaultNames("personal", "canary-env", { variable: "API_KEY", value: null })
  expect(requests.at(-1)).toMatchObject({ variable: "API_KEY", value: "", delete: true })
})

test("vaults are listed and deleted by name in the workspace", async () => {
  const listed = { vaults: [{ name: "a", key_epoch: 1, revision: 2, wraps: [], recipes: ["ci"] }] }
  globalThis.fetch = (async (input: string | URL, init?: RequestInit) => {
    fetched.push({ url: new URL(String(input), "http://console.example.test").pathname, method: init?.method ?? "GET" })
    return init?.method === "DELETE"
      ? new Response(null, { status: 204 })
      : Response.json({ data: listed })
  }) as typeof fetch
  expect(await listNamedVaults("acme")).toEqual(listed.vaults)
  await deleteNamedVault("acme", "a b")
  expect(fetched).toEqual([
    { url: "/api/v1/workspaces/acme/env-vaults", method: "GET" },
    { url: "/api/v1/workspaces/acme/env-vaults/a%20b", method: "DELETE" },
  ])
})
