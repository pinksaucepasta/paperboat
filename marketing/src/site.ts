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
export const installUrl = `${config.installer_base_url}/install`
export const selfhostInstallUrl = `${config.installer_base_url}/selfhost/install`
export const localDomain = config.local_access_base_domain
export const previewDomain = config.preview_base_domain
export const tunnelDomain = config.tunnel_base_domain

/** The public source repository (LICENSE: MIT and AGPL-3.0). */
export const sourceUrl = "https://github.com/pinksaucepasta/paperboat"

export const installCommand = `curl -fsSL ${installUrl} | bash`
/** The Windows installer (server /install.ps1), run in PowerShell. */
export const windowsInstallCommand = `irm ${installUrl}.ps1 | iex`

/** Plans and prices (PLAN 1.3): the one place the site states them. */
export const plans = [
  {
    name: "Free",
    price: "$0",
    yearly: "$0",
    unit: "",
    bandwidth: "5 GB",
    machines: "100",
    publications: "100",
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
    publications: "1,000",
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

/** Extra bandwidth (PLAN 1.3): bought in 100 GB units for a paid workspace, used after the plan's own allowance. */
export const topUp = { size: "100 GB", price: "$5" }

/** Audit log retention per plan (server config business.audit_retention_*; server/docs/teams-business.md). */
export const auditRetention = ["30 days", "90 days", "365 days"] as const

/** Single sign-on and directory sync need a team of at least this many seats (server config business.sso_min_seats). */
export const ssoMinSeats = 10

/** Public raw-TCP tunnel addresses per plan (server config billing.limits.public_tcp). */
export const publicTcp = ["1", "10", "10 per seat"] as const

/** The Team plan's trial, said once wherever it is mentioned. */
export const teamTrial =
  "Team starts with a 14-day trial. Your card is taken at the start and billing begins on day 15. We remind you 3 days before, and cancelling during the trial costs nothing."

