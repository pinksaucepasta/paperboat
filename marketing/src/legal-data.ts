import { plans, speeds } from "./site"

/**
 * The legal pages, and the facts only the company can supply. Every `TODO-USER` value below must be filled in
 * before launch; until then each legal page shows it highlighted and stays out of search (noindex).
 */
export type Legal = { slug: string; title: string; short: string; updated: string }

/** Date the current text of each page was written (ISO). Change it whenever the page's meaning changes. */
const updated = "2026-11-19"

export const legalPages: Legal[] = [
  { slug: "terms", title: "Terms of Service", short: "Terms", updated },
  { slug: "privacy", title: "Privacy Policy", short: "Privacy", updated },
  { slug: "acceptable-use", title: "Acceptable Use Policy", short: "Acceptable use", updated },
  { slug: "refunds", title: "Refund Policy", short: "Refunds", updated },
  { slug: "subprocessors", title: "Subprocessors", short: "Subprocessors", updated },
  { slug: "dpa", title: "Data Processing Addendum", short: "DPA", updated },
]

export const legalPage = (slug: string) => legalPages.find((page) => page.slug === slug)!

/** Facts about the operating company. Only the company can supply these; never guess them. */
export const company = {
  /** The two individuals who operate Paperboat (user decision 2026-11-19). */
  /** EU consumer law (Directive 2011/83/EU art. 6(1)(b)-(c)) needs the trader's legal name and postal address. */
  operators: "Pujan Modha and Anvit Dadape",
  /** Postal address for legal notices and the DMCA agent (user, 2026-11-19). */
  address: "3869 Miramar Street #4208, La Jolla, CA 92092, USA",
  law: "the State of California, United States, without regard to its conflict-of-law rules",
  courts: "the state and federal courts located in California",
  legalEmail: "legal@mail.paperboat.cloud",
  abuseEmail: "abuse@mail.paperboat.cloud",
  privacyEmail: "privacy@mail.paperboat.cloud",
  /** The user's final answer (2026-11-19). */
  effective: "14 November 2026",
}

/** True while any company fact is still missing: the pages are then drafts. */
export const legalDraft = Object.values(company).some((value) => value.startsWith("TODO-USER"))

/** Fair use for the legal pages, from the plans and speeds in site.ts: edge and relay share one monthly allowance (per seat on Team). */
export const fairUse = {
  free: { allowance: plans[0].bandwidth, slow: speeds.after[0] },
  paid: { allowance: plans[1].bandwidth, slow: speeds.after[1] },
  relaySpeed: speeds.relay,
  /** Typing and output in browser terminals do not count toward the allowance and are capped at this speed per workspace; console uploads count (user decision 2026-11-19). */
  terminalSpeed: "10 Mbit/s",
}

const inBytes: Record<string, string> = { "512 kbit/s": "about 64 KB/s", "1 Mbit/s": "about 125 KB/s", "10 Mbit/s": "about 1.25 MB/s" }
/** "512 kbit/s (about 64 KB/s)": a speed with its bytes figure, for readers who think in file sizes. */
export const withBytes = (speed: string) => (inBytes[speed] ? `${speed} (${inBytes[speed]})` : speed)
