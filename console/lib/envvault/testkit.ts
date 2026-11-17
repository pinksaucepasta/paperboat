import { readFileSync } from "node:fs"
import { createRequire } from "node:module"
import { join } from "node:path"
import type { Vault } from "@/lib/envvault/vault"

// The browser's Worker stood in by the real vault WASM in this process, and a vault the CLI's code sealed
// (shared/testdata/contracts/environment-e2ee-v1/cli-vault.json), for tests of the console's own vault code.

const fixturePath = join(import.meta.dir, "../../../shared/testdata/contracts/environment-e2ee-v1/cli-vault.json")

export const fixture = JSON.parse(readFileSync(fixturePath, "utf8")) as {
  password: string
  recovery_code: string
  vault: Vault
}

const wasm = (json: string) =>
  (globalThis as unknown as { paperboatEnvironmentVault: (json: string) => Promise<string> }).paperboatEnvironmentVault(
    json,
  )

/** A Worker that runs the module in this process; terminating it is the lock, as in a browser. */
export class WasmWorker {
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

export async function loadVaultWasm() {
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
}
