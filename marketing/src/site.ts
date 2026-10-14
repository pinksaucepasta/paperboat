/**
 * Addresses the site links to. The installer URLs are the ones the console hands out
 * (paperboat-console/lib/enrollment.ts, lib/network.ts); the console origin is set here once.
 */
export const consoleUrl = "https://console.pprbt.dev"
export const installUrl = "https://get.pprbt.dev/install"
export const selfhostInstallUrl = "https://get.pprbt.dev/selfhost/install"

export const installCommands = {
  posix: `curl -fsSL ${installUrl} | bash`,
  powershell: `powershell -c "iex (irm '${installUrl}')"`,
} as const

/** The mark: a paper boat on a 10 × 6 cell grid, 60 units per cell (brand guidelines 02). */
export const boatPath =
  "M270 0 H330 V60 H390 V120 H450 V180 H150 V120 H210 V60 H270 Z M0 180 H120 V240 H480 V180 H600 V240 H540 V300 H480 V360 H120 V300 H60 V240 H0 Z"
