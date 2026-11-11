import { afterEach, beforeAll, beforeEach, expect, test } from "bun:test"
import { createHash } from "node:crypto"
import { readFileSync } from "node:fs"
import { createRequire } from "node:module"
import { join } from "node:path"
import { lockVault, vaultUnlocked } from "@/lib/envvault/crypto"
import {
  changePassword,
  newRecoveryCode,
  recoverVault,
  recoveryEnabled,
  replaceRecoveryCode,
  setUpVault,
} from "@/lib/envvault/protection"
import { unlockVault, type Vault } from "@/lib/envvault/vault"

// The console's own vault code (crypto.ts worker client, vault.ts, protection.ts) on the real vault WASM, against a
// vault the CLI's code sealed (testdata/cli-vault.json, checked natively by TestConsoleFixtureIsANativeVaultWithRecovery
// in cli/cmd/environmentvault-wasm). Only the browser's Worker and fetch are stood in for.

const fixture = JSON.parse(readFileSync(join(import.meta.dir, "testdata", "cli-vault.json"), "utf8")) as {
  password: string
  recovery_code: string
  vault: Vault
}

const wasm = (json: string) =>
  (globalThis as unknown as { paperboatEnvironmentVault: (json: string) => Promise<string> }).paperboatEnvironmentVault(
    json,
  )

/** A Worker that runs the module in this process; terminating it is the lock, as in a browser. */
class WasmWorker {
  onmessage?: (event: { data: { id: number; reply: string } }) => void
  onerror?: () => void
  constructor(readonly url: string) {}
  postMessage({ id, request }: { id: number; request: string }) {
    void wasm(request).then((reply) => this.onmessage?.({ data: { id, reply } }))
  }
  terminate() {
    void wasm(JSON.stringify({ action: "lock" }))
  }
}

beforeAll(async () => {
  const assets = join(import.meta.dir, "..", "..", "public", "environment-vault")
  createRequire(import.meta.url)(join(assets, "wasm_exec.js"))
  const go = new (
    globalThis as unknown as {
      Go: new () => { importObject: WebAssembly.Imports; run(i: WebAssembly.Instance): Promise<void> }
    }
  ).Go()
  const { instance } = await WebAssembly.instantiate(
    readFileSync(join(assets, "environment-vault.wasm")),
    go.importObject,
  )
  void go.run(instance)
  for (let i = 0; i < 500 && !(globalThis as { paperboatEnvironmentVault?: unknown }).paperboatEnvironmentVault; i++) {
    await new Promise((r) => setTimeout(r, 10))
  }
})

const realFetch = globalThis.fetch
const realWorker = (globalThis as { Worker?: unknown }).Worker
let stored: Vault | null
let account: string
let putFails: "never" | "before" | "after"
let sent: string[]

/** The control plane's vault endpoints, enough for the console: it names the issuer when no vault exists and chains generations. */
beforeEach(() => {
  ;(globalThis as { Worker: unknown }).Worker = WasmWorker
  ;(globalThis as { addEventListener?: unknown }).addEventListener ??= () => {}
  stored = structuredClone(fixture.vault)
  account = fixture.vault.account_id
  putFails = "never"
  sent = []
  globalThis.fetch = (async (input: string | URL, init?: RequestInit) => {
    const url = new URL(String(input), "http://console.example.test")
    sent.push(`${init?.method ?? "GET"} ${url.pathname} ${typeof init?.body === "string" ? init.body : ""}`)
    if (url.pathname === "/api/v1/me") return Response.json({ data: { id: account } })
    if (url.pathname !== "/api/v1/environment-vault") return new Response(null, { status: 404 })
    if (init?.method === "PUT") {
      if (putFails === "before") return Response.json({ error: { code: "unavailable" } }, { status: 500 })
      const { envelope } = JSON.parse(init.body as string) as { envelope: string }
      const raw = Buffer.from(envelope, "base64url")
      stored = {
        issuer: fixture.vault.issuer,
        account_id: account,
        generation: (stored?.generation ?? 0) + 1,
        document_id: `sha256:${createHash("sha256").update(raw).digest("hex")}`,
        envelope,
      }
      if (putFails === "after") return Response.json({ error: { code: "unavailable" } }, { status: 500 })
      return Response.json({ data: stored })
    }
    if (!stored) {
      return Response.json(
        { error: { code: "not_found", message: "none", details: { issuer: fixture.vault.issuer } } },
        { status: 404 },
      )
    }
    return Response.json({ data: stored })
  }) as typeof fetch
})

afterEach(() => {
  lockVault()
  globalThis.fetch = realFetch
  ;(globalThis as { Worker?: unknown }).Worker = realWorker
})

const opens = (secret: string, kind: "password" | "recovery") =>
  unlockVault(secret, kind).then(
    () => {
      lockVault()
      return true
    },
    () => false,
  )

test("a vault the CLI made recovers in the browser: new password and new code, the old ones stop working", async () => {
  await unlockVault(fixture.recovery_code, "recovery")
  expect(vaultUnlocked()).toBe(false) // the code alone does not open the page's vault
  const code = await newRecoveryCode()
  expect(code).toMatch(/^pbvr1-[A-Z2-7]{58}$/)
  await recoverVault("a new password", code)
  expect(vaultUnlocked()).toBe(true)
  expect(stored?.generation).toBe(fixture.vault.generation + 1) // one version, not one per secret
  lockVault()
  expect(await opens(fixture.password, "password")).toBe(false)
  expect(await opens(fixture.recovery_code, "recovery")).toBe(false)
  expect(await opens("a new password", "password")).toBe(true)
  expect(await opens(code, "recovery")).toBe(true)
})

test("neither a password nor a recovery code is ever sent to the server", async () => {
  await unlockVault(fixture.recovery_code, "recovery")
  const code = await newRecoveryCode()
  await recoverVault("secret new password", code)
  await changePassword("another secret password")
  await replaceRecoveryCode(await newRecoveryCode())
  const requests = sent.join("\n")
  for (const secret of [
    fixture.password,
    fixture.recovery_code,
    "secret new password",
    "another secret password",
    code,
  ]) {
    expect(requests).not.toContain(secret)
  }
})

test("a wrong recovery code opens nothing", async () => {
  expect(await opens("pbvr1-" + "A".repeat(58), "recovery")).toBe(false)
  expect(vaultUnlocked()).toBe(false)
})

test("the unlocked vault changes its password, replaces its recovery code and turns it off", async () => {
  await unlockVault(fixture.password)
  expect(await recoveryEnabled()).toBe(true)
  await changePassword("changed password")
  expect(await opens(fixture.password, "password")).toBe(false)
  expect(await opens("changed password", "password")).toBe(true)

  await unlockVault("changed password")
  const replacement = await newRecoveryCode()
  await replaceRecoveryCode(replacement)
  expect(await opens(fixture.recovery_code, "recovery")).toBe(false)
  expect(await opens(replacement, "recovery")).toBe(true)

  await unlockVault("changed password")
  await replaceRecoveryCode("")
  expect(await recoveryEnabled()).toBe(false)
  expect(await opens(replacement, "recovery")).toBe(false)
  expect(vaultUnlocked()).toBe(true)

  const code = await newRecoveryCode() // and back on
  await replaceRecoveryCode(code)
  expect(await recoveryEnabled()).toBe(true)
  expect(await opens(code, "recovery")).toBe(true)
})

test("without a vault the browser creates one with the issuer the server names, and it recovers", async () => {
  stored = null
  const code = await newRecoveryCode()
  await setUpVault("first password", code)
  expect(vaultUnlocked()).toBe(true)
  expect(stored).toMatchObject({ issuer: fixture.vault.issuer, account_id: account, generation: 1 })
  lockVault()
  expect(await opens("first password", "password")).toBe(true)
  expect(await opens(code, "recovery")).toBe(true)
  expect(await opens("other password", "password")).toBe(false)
})

test("a refused publication drops the browser's keys and changes nothing", async () => {
  await unlockVault(fixture.password)
  putFails = "before"
  await expect(changePassword("never used")).rejects.toBeDefined()
  expect(vaultUnlocked()).toBe(false)
  expect(stored?.envelope).toBe(fixture.vault.envelope)
  expect(await opens(fixture.password, "password")).toBe(true)
})

test("a lost response to a publication that went through still opens the vault", async () => {
  await unlockVault(fixture.password)
  putFails = "after"
  await changePassword("kept password")
  expect(vaultUnlocked()).toBe(true)
  expect(stored?.generation).toBe(fixture.vault.generation + 1)
  lockVault()
  expect(await opens("kept password", "password")).toBe(true)
})
