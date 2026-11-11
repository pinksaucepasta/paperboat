import { beforeAll, expect, test } from "bun:test"
import { readFileSync } from "node:fs"
import { createRequire } from "node:module"
import { join } from "node:path"
import { recordingVaultRequest, toBase64 } from "@/lib/recordings"
import { recordingArgs } from "@/lib/terminal/browser-terminal-wasm"

// The console talks to two Go WASM modules (cli/cmd/browserterminal-wasm, cli/cmd/environmentvault-wasm) through
// untyped calls. These load the modules `bun run check` builds and call them exactly as the console does, so a
// change of arguments or encoding on either side fails here instead of in a browser.

type Reply = { error?: string; message?: string; result?: unknown }
const terminal = () => (globalThis as unknown as { paperboatBrowserTerminal: Record<string, (...args: unknown[]) => unknown> }).paperboatBrowserTerminal
const vault = (json: string) =>
  (globalThis as unknown as { paperboatEnvironmentVault: (json: string) => Promise<string> }).paperboatEnvironmentVault(json)

async function start(dir: string, wasm: string, ready: () => unknown) {
  const assets = join(import.meta.dir, "..", "public", dir)
  createRequire(import.meta.url)(join(assets, "wasm_exec.js"))
  const go = new (globalThis as unknown as { Go: new () => { importObject: WebAssembly.Imports; run(i: WebAssembly.Instance): Promise<void> } }).Go()
  const { instance } = await WebAssembly.instantiate(readFileSync(join(assets, wasm)), go.importObject)
  void go.run(instance)
  for (let i = 0; i < 500 && !ready(); i++) await new Promise((r) => setTimeout(r, 10))
  if (!ready()) throw new Error(`${wasm} did not start; run bun run terminal:wasm and bun run vault:wasm`)
}

beforeAll(async () => {
  // Each module ships its own wasm_exec.js (the vault may be TinyGo's); load one after the other.
  await start("terminal", "browserterminal.wasm", () => (globalThis as { paperboatBrowserTerminal?: unknown }).paperboatBrowserTerminal)
  await start("environment-vault", "environment-vault.wasm", () => (globalThis as { paperboatEnvironmentVault?: unknown }).paperboatEnvironmentVault)
})

const recordingID = "rec_00000000-0000-4000-8000-000000000000"

test("the terminal module takes recordingRead and recordingControl with the console's arguments", async () => {
  // A valid call reaches the connection lookup (none here); wrong arguments are refused before it.
  const read = (await terminal().recordingRead(...recordingArgs.read("connection_missing", recordingID))) as Reply
  expect(read.error).toBe("connection_closed")
  const control = (await terminal().recordingControl(...recordingArgs.control("connection_missing", "record-start"))) as Reply
  expect(control.error).toBe("connection_closed")
})

test("the vault module makes a recording key from the console's request and answers in base64url", async () => {
  const pair = (await crypto.subtle.generateKey({ name: "X25519" }, true, ["deriveBits"])) as CryptoKeyPair
  const sharing = new Uint8Array(await crypto.subtle.exportKey("raw", pair.publicKey))
  const request = recordingVaultRequest({
    action: "recording-key-create",
    team_id: "team-check",
    viewers: [{ account_id: "user_check", sharing_public_key: toBase64(sharing) }],
  })
  const reply = JSON.parse(await vault(JSON.stringify(request))) as {
    data?: { key: { generation: number; public_key: string }; grants: { account_id: string; sealed: string }[] }
    error?: string
  }
  expect(reply.error).toBeUndefined()
  expect(reply.data?.key.generation).toBe(1)
  expect(reply.data?.grants.map((grant) => grant.account_id)).toEqual(["user_check"])
  expect(reply.data?.key.public_key).toMatch(/^[A-Za-z0-9_-]{43}$/)
})

test("the vault module takes a segment file as base64url inside the one JSON request", async () => {
  // An unknown handle is refused by the engine itself; a request it can't read answers "Invalid ENV operation."
  const request = recordingVaultRequest({ action: "recording-segment", handle: "handle_missing", file: toBase64(new Uint8Array(4096).fill(250)) })
  expect(request.file).toMatch(/^[A-Za-z0-9_-]+$/)
  const reply = JSON.parse((await vault(JSON.stringify(request))) ?? "null") as { error?: string } | null
  expect(reply?.error).toBeDefined()
  expect(reply?.error).not.toBe("Invalid ENV operation.")
})
