/**
 * The compare pages: one entry per product, one template (pages/compare/[slug].astro). Every
 * text here is a placeholder until the copy is written. TODO-COPY marks each one, so the whole
 * site can be searched for what is still open. Nothing here may claim anything about another
 * product that has not been checked against its own documentation.
 */
export type CompareRow = { feature: string; them: string; us: string }

export type Compare = {
  slug: string
  name: string
  /** The page title and the one line under it. */
  title: string
  summary: string
  rows: CompareRow[]
  themWhen: string[]
  usWhen: string[]
  switching: string[]
}

const todo = (what: string) => `TODO-COPY: ${what}`

/** The rows every comparison answers, in this order. */
const rowNames = [
  "Remote terminal",
  "SSH and file copy",
  "Public links for local ports",
  "Permanent addresses and your own domains",
  "Private network between machines",
  "End-to-end encryption",
  "Teams, roles and sharing",
  "Self-hosting",
  "Pricing",
]

function entry(slug: string, name: string): Compare {
  return {
    slug,
    name,
    title: `Paperboat vs ${name}`,
    summary: todo(`one plain line on how ${name} and Paperboat differ`),
    rows: rowNames.map((feature) => ({
      feature,
      them: todo(`what ${name} does for “${feature}”, checked against its docs`),
      us: todo(`what Paperboat does for “${feature}”`),
    })),
    themWhen: [todo(`when ${name} is the better pick`), todo("a second reason")],
    usWhen: [todo("when Paperboat is the better pick"), todo("a second reason")],
    switching: [todo(`step 1 of moving from ${name}`), todo("step 2"), todo("step 3")],
  }
}

export const compares: Compare[] = [
  entry("tailscale", "Tailscale"),
  entry("ngrok", "ngrok"),
  entry("cloudflare-tunnel", "Cloudflare Tunnel"),
  entry("teleport", "Teleport"),
  entry("netbird", "NetBird"),
]
