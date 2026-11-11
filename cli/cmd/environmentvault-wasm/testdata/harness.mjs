// Drives a built environment-vault wasm module: one JSON request per stdin line,
// one JSON response per stdout line. Used by interop_test.go only.
import { readFileSync } from "node:fs"
import { createInterface } from "node:readline"
import { createRequire } from "node:module"

const [wasmPath, execPath] = process.argv.slice(2)
createRequire(import.meta.url)(execPath)
const go = new globalThis.Go()
const { instance } = await WebAssembly.instantiate(readFileSync(wasmPath), go.importObject)
void go.run(instance)
while (!globalThis.paperboatEnvironmentVault) await new Promise((r) => setTimeout(r, 10))
for await (const line of createInterface({ input: process.stdin })) {
  if (line.trim()) process.stdout.write((await globalThis.paperboatEnvironmentVault(line)) + "\n")
}
