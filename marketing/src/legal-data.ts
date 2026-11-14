/**
 * The legal pages. The legal text itself is not written yet (it needs the operating company's
 * details), so each page says so and points to what is already published.
 */
export type Legal = { slug: string; title: string; short: string; covers: string }

export const legalPages: Legal[] = [
  { slug: "terms", title: "Terms of service", short: "Terms", covers: "the rules for using the hosted service and the official pb builds, including use at work" },
  { slug: "privacy", title: "Privacy policy", short: "Privacy", covers: "what personal data Paperboat holds, why, for how long, and your rights over it" },
  { slug: "dpa", title: "Data processing agreement", short: "DPA", covers: "how Paperboat processes personal data on behalf of a team, for customers who need a signed agreement" },
  { slug: "acceptable-use", title: "Acceptable use policy", short: "Acceptable use", covers: "what public previews and tunnels may not be used for, such as phishing or distributing malware" },
]
