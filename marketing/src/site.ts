/**
 * Everything the site says about addresses comes from the server's client configuration
 * (`GET /v1/client-configuration`), read when the site builds or the dev server starts; the only
 * setting is the API address, `PAPERBOAT_API_URL` (see .env.example). No domain is written in code.
 */
import { statusUrl as resolveStatusUrl } from "./status-url.mjs"
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
  abuse_mailbox?: string
}

export const apiBaseUrl = apiUrl
/** Where abuse is reported by email; undefined when the server publishes none. */
export const abuseMailbox = config.abuse_mailbox
export const consoleUrl = config.console_url
/** The status page's address (PAPERBOAT_STATUS_URL, else status.<site domain> for an https site), when this deployment has one. */
export const statusUrl: string | undefined = resolveStatusUrl(import.meta.env.PAPERBOAT_SITE_URL, import.meta.env.PAPERBOAT_STATUS_URL)
/** Where the installers are served, e.g. https://get.example.test. */
export const installerBase = config.installer_base_url
export const selfhostInstallUrl = `${config.installer_base_url}/selfhost`
export const localDomain = config.local_access_base_domain
export const previewDomain = config.preview_base_domain
export const tunnelDomain = config.tunnel_base_domain

/** The public source repository (LICENSE: MIT and AGPL-3.0). */
export const sourceUrl = "https://github.com/pinksaucepasta/paperboat"

/** One address: it serves install.sh to curl and install.ps1 to PowerShell. */
export const installCommand = `curl -fsSL ${installerBase} | sh`
export const windowsInstallCommand = `irm ${installerBase} | iex`

/** Plans and prices (PLAN 1.3): the one place the site states them. */
export const plans = [
  {
    name: "Free",
    price: "$0",
    yearly: "$0",
    unit: "",
    bandwidth: "5 GB",
    machines: "100",
    publications: "10",
    browserTerminals: "10",
    detail: "Every feature for personal use, on your own machines.",
  },
  {
    name: "Pro",
    price: "$8",
    yearly: "$80",
    unit: "",
    bandwidth: "100 GB",
    machines: "1,000",
    publications: "100",
    browserTerminals: "100",
    detail: "Everything in Free, with more bandwidth. Add 100 GB any time.",
  },
  {
    name: "Team",
    price: "$15",
    yearly: "$150",
    unit: "a seat",
    bandwidth: "100 GB a seat",
    machines: "Unlimited",
    publications: "Unlimited",
    browserTerminals: "Unlimited",
    detail: "Members, and lasting access to each other's machines. Try it free for 14 days.",
  },
] as const

/**
 * Traffic through our servers is never cut off (user decision 2026-11-19): once the plan's
 * bandwidth is used it is slowed until the month resets. Relays also have a top speed.
 */
/** A plan's monthly bandwidth and the speed after it, for the pricing tables: "100 GB/seat/mo, then 1 Mbit/s". */
export const bandwidthCell = (i: number) =>
  `${plans[i].bandwidth.replace(" a seat", "/seat")}/mo, then ${speeds.after[i]}`

export const speeds = {
  /** Once the plan's bandwidth is used: Free, Pro, Team. */
  after: ["512 kbit/s", "1 Mbit/s", "1 Mbit/s"],
  /** Relayed traffic before that: Free, Pro, Team. */
  relay: ["10 Mbit/s", "100 Mbit/s", "100 Mbit/s"],
} as const

/** Extra bandwidth (PLAN 1.3): bought in 100 GB units for a paid workspace, used after the plan's own allowance. */
export const topUp = { size: "100 GB", price: "$5" }

/** Audit log retention per plan (server config business.audit_retention_*; server/docs/teams-business.md). */
export const auditRetention = ["30 days", "90 days", "365 days"] as const

/** SSO and SCIM are paid add-ons on a Team at any size, dollars a month (server config business.sso_addon_cents,
 * scim_addon_cents, sso_scim_addon_cents); a year costs addon_yearly_months months. */
export const addons = { sso: 100, scim: 100, both: 150, yearlyMonths: 10 }
export const addonPrices = `SSO $${addons.sso}/mo, SCIM $${addons.scim}/mo, both $${addons.both}/mo`

/** Public raw-TCP tunnel addresses per plan (server config billing.limits.public_tcp). */
export const publicTcp = ["1", "10", "10 per seat"] as const


/** The mark: a paper boat on a 10 × 6 cell grid, 60 units per cell (brand guidelines 02). */
export const boatPath =
  "M270 0 H330 V60 H390 V120 H450 V180 H150 V120 H210 V60 H270 Z M0 180 H120 V240 H480 V180 H600 V240 H540 V300 H480 V360 H120 V300 H60 V240 H0 Z"
