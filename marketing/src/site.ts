/**
 * Everything the site says about addresses comes from the server's client configuration
 * (`GET /v1/client-configuration`), read when the site builds or the dev server starts; the only
 * setting is the API address, `PAPERBOAT_API_URL` (see .env.example). No domain is written in code.
 */
const apiUrl = import.meta.env.PAPERBOAT_API_URL
if (!apiUrl) throw new Error("PAPERBOAT_API_URL is required (see marketing/.env.example)")

const response = await fetch(new URL("/v1/client-configuration", apiUrl), { signal: AbortSignal.timeout(10_000) })
if (!response.ok) throw new Error(`Client configuration from ${apiUrl} failed with HTTP ${response.status}`)
const config = (await response.json()).data as {
  console_url: string
  installer_base_url: string
  local_access_base_domain: string
  preview_base_domain: string
  tunnel_base_domain: string
}

export const consoleUrl = config.console_url
export const installUrl = `${config.installer_base_url}/install`
export const selfhostInstallUrl = `${config.installer_base_url}/selfhost/install`
export const localDomain = config.local_access_base_domain
export const previewDomain = config.preview_base_domain
export const tunnelDomain = config.tunnel_base_domain

/** Launch platforms (PLAN 1.1): macOS on Apple Silicon and Linux; Windows follows after launch. */
export const installCommand = `curl -fsSL ${installUrl} | bash`

/** Plans and prices (PLAN 1.3): the one place the site states them. */
export const plans = [
  {
    name: "Free",
    price: "$0",
    period: "",
    bandwidth: "5 GB",
    detail: "Everything for one person, on your own machines.",
  },
  {
    name: "Pro",
    price: "$8",
    period: "/month, or $80/year",
    bandwidth: "100 GB",
    detail: "Everything in Free, with more bandwidth. Add 100 GB any time.",
  },
  {
    name: "Team",
    price: "$15",
    period: "/seat/month, or $150/seat/year",
    bandwidth: "100 GB per seat",
    detail: "Members, and lasting access to each other's machines. Try it free for 14 days.",
  },
] as const

/** The Team plan's trial, said once wherever it is mentioned. */
export const teamTrial =
  "Team starts with a 14-day trial. Your card is taken at the start and billing begins on day 15. We remind you 3 days before, and cancelling during the trial costs nothing."

/** The mark: a paper boat on a 10 × 6 cell grid, 60 units per cell (brand guidelines 02). */
export const boatPath =
  "M270 0 H330 V60 H390 V120 H450 V180 H150 V120 H210 V60 H270 Z M0 180 H120 V240 H480 V180 H600 V240 H540 V300 H480 V360 H120 V300 H60 V240 H0 Z"
