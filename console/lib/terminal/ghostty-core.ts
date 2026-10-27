import { GhosttyCore, type GhosttyOptions } from "@wterm/ghostty"

// GhosttyCore.load() fetches and instantiates in one async step and its constructor is private. A synchronous
// restore needs both, so this rebuilds what load() does from a module compiled once.
const construct = GhosttyCore as unknown as new (
  wasm: { exports: WebAssembly.Exports; instance: WebAssembly.Instance },
  options: GhosttyOptions,
) => GhosttyCore

// Ghostty's wasm memory is never cleared: a terminal reset or re-created inside one instance keeps the cells of
// the screen before it, and they come back in rows a later reflow rebuilds. Every screen restore therefore
// starts on a fresh instance, which needs this factory to be synchronous.
export async function loadGhosttyCores(wasmPath: string): Promise<(options: GhosttyOptions) => GhosttyCore> {
  const response = await fetch(wasmPath)
  if (!response.ok) throw new Error(`fetching ${wasmPath} returned ${response.status}`)
  const compiled = await WebAssembly.compile(await response.arrayBuffer())
  return (options) => {
    const instance = new WebAssembly.Instance(compiled, { env: { log() {} } })
    return new construct({ exports: instance.exports, instance }, { ...options, wasmPath })
  }
}
