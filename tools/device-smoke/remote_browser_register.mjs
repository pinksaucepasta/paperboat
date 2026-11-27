// node --import this file: console scripts that import "playwright-core" by name get remote_browser.mjs too.
import { registerHooks } from "node:module"

const shim = new URL("./remote_browser.mjs", import.meta.url).href
registerHooks({
  resolve(specifier, context, nextResolve) {
    if ((specifier === "playwright-core" || specifier === "playwright") && context.parentURL !== shim) return { url: shim, shortCircuit: true }
    return nextResolve(specifier, context)
  },
})
