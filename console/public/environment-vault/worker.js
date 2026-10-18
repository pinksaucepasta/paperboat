// Runs the ENV vault WASM (cli/cmd/environmentvault-wasm) off the page's thread. Keys live only
// in this worker's WASM memory; terminating the worker is the lock. Built assets (gitignored):
// environment-vault.wasm and wasm_exec.js, from scripts/build-environment-vault-wasm.sh.
importScripts("/environment-vault/wasm_exec.js")

const ready = (async () => {
  const go = new Go()
  const response = await fetch("/environment-vault/environment-vault.wasm", { cache: "no-store" })
  if (!response.ok) throw new Error("ENV cryptography is unavailable.")
  const { instance } = await WebAssembly.instantiate(await response.arrayBuffer(), go.importObject)
  void go.run(instance)
  for (let i = 0; i < 500 && !self.paperboatEnvironmentVault; i++) await new Promise((r) => setTimeout(r, 10))
  if (!self.paperboatEnvironmentVault) throw new Error("ENV cryptography did not start.")
})()

self.onmessage = async ({ data }) => {
  try {
    await ready
    postMessage({ id: data.id, reply: await self.paperboatEnvironmentVault(data.request) })
  } catch (error) {
    postMessage({
      id: data.id,
      reply: JSON.stringify({ error: error instanceof Error ? error.message : "ENV cryptography failed." }),
    })
  }
}
