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
  /** Until Paperboat, Inc. (a Delaware C corporation) is formed, these two individuals operate Paperboat (user decision 2026-11-19). */
  /** EU consumer law (Directive 2011/83/EU art. 6(1)(b)-(c)) and India's E-Commerce Rules 2020 (rule 5(3)(a)) need the trader's legal name and postal address. */
  operators: "Pujan Modha and Anvit Dadape",
  /** Required only where consumer law needs a geographical address (Terms, Refund Policy); never a home address. */
  address: "TODO-USER: postal address for legal notices (not a home address)",
  law: "the State of California, United States, without regard to its conflict-of-law rules",
  courts: "the state and federal courts located in California",
  legalEmail: "legal@mail.paperboat.cloud",
  privacyEmail: "privacy@mail.paperboat.cloud",
  /** GDPR art. 27 (and UK GDPR): a controller outside the EU/UK that serves people there names a representative in each. */
  euRepresentative: "TODO-USER: EU and UK representative (name and address)",
  /** India's Consumer Protection (E-Commerce) Rules 2020 require a named grievance officer. */
  grievanceOfficer: "TODO-USER: grievance officer name",
  /** The date these terms take effect, e.g. "20 November 2026". */
  effective: "TODO-USER: effective date",
}

/** True while any company fact is still missing: the pages are then drafts. */
export const legalDraft = Object.values(company).some((value) => value.startsWith("TODO-USER"))

/**
 * Fair use for traffic through servers Paperboat runs (user-approved 2026-11-19, built by the BANDWIDTH lane): a monthly amount at
 * full speed, then a slow rate until the month resets. Edge and relay are counted separately; browser terminals count as edge
 * traffic; on Team the paid amounts are per seat. Self-hosted relays and edges are not limited.
 */
export const fairUse = {
  free: { edge: "5 GB", relay: "5 GB", slow: "512 kbit/s" },
  paid: { edge: "100 GB", relay: "100 GB", slow: "1 Mbit/s" },
  relaySpeed: "10 Mbit/s",
}
