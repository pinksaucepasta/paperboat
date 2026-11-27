// Drop-in PW_MODULE: every console script's `chromium.launch()` becomes a browser on another host (console_browser.sh),
// while the script, its session file and its secrets stay on the controller. Without PW_REMOTE_FILE it is the real module.
//   PW_MODULE=<this file> PW_REAL_MODULE=<playwright-core index.mjs> [PW_REMOTE_FILE=<mode 600 file holding the ws URL>]
import { readFileSync } from "node:fs"

const real = await import(process.env.PW_REAL_MODULE ?? "playwright-core")
const endpoint = process.env.PW_REMOTE_FILE ? readFileSync(process.env.PW_REMOTE_FILE, "utf8").trim() : ""
const remote = (type) =>
  new Proxy(type, {
    get(target, name) {
      // executablePath and channel name a local browser: the server runs its own build. args and headless still apply.
      if (name === "launch")
        return (options = {}) => {
          const { executablePath, channel, ...rest } = options
          return target.connect(endpoint, { timeout: 60000, headers: { "x-playwright-launch-options": JSON.stringify(rest) } })
        }
      if (name === "launchPersistentContext" || name === "launchServer") return () => { throw new Error(`${String(name)} is not available with a remote browser`) }
      const value = target[name]
      return typeof value === "function" ? value.bind(target) : value
    },
  })

export const chromium = endpoint ? remote(real.chromium) : real.chromium
export const webkit = endpoint ? remote(real.webkit) : real.webkit
export const firefox = endpoint ? remote(real.firefox) : real.firefox
export const devices = real.devices
export const request = real.request
export const errors = real.errors
export const selectors = real.selectors
export default { chromium, webkit, firefox, devices, request, errors, selectors }
