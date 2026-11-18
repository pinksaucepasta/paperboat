/**
 * The legal pages, and the facts only the company can supply. Every `TODO-USER` value below must be filled in
 * before launch; until then each legal page shows it highlighted and stays out of search (noindex).
 */
export type Legal = { slug: string; title: string; short: string; updated: string }

/** Date the current text of each page was written (ISO). Change it whenever the page's meaning changes. */
const updated = "2026-11-18"

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
  name: "TODO-USER: legal entity name",
  address: "TODO-USER: registered address",
  /** The country whose law governs the Terms, e.g. "India". */
  law: "TODO-USER: governing law",
  /** The courts with jurisdiction, e.g. "the courts of Bengaluru, India". */
  courts: "TODO-USER: courts / venue",
  legalEmail: "TODO-USER: legal@ address",
  privacyEmail: "TODO-USER: privacy@ address",
  /** India's Consumer Protection (E-Commerce) Rules 2020 require a named grievance officer. */
  grievanceOfficer: "TODO-USER: grievance officer name",
  /** The date these terms take effect, e.g. "20 November 2026". */
  effective: "TODO-USER: effective date",
}

/** True while any company fact is still missing: the pages are then drafts. */
export const legalDraft = Object.values(company).some((value) => value.startsWith("TODO-USER"))

/** Fair use for traffic through servers Paperboat runs (PLAN 1.3, user decision): a monthly amount at full speed, then a slow rate. */
export const fairUse = {
  edge: { free: "5 GB", paid: "100 GB", slowFree: "512 kbit/s", slowPaid: "1 Mbit/s" },
  relay: { speed: "10 Mbit/s", free: "5 GB", paid: "100 GB" },
}
