import { consoleUrl, sourceUrl } from "./site"
import { compares } from "./compare-data"
import { features, useCases } from "./features"
import { legalPages } from "./legal-data"

/** The site's navigation, in one place: the header and the footer both read it. */
export type NavLink = { href: string; label: string }

export const featureLinks: NavLink[] = features.map((item) => ({ href: `/features/${item.slug}/`, label: item.name }))
export const useCaseLinks: NavLink[] = useCases.map((item) => ({ href: `/use-cases/${item.slug}/`, label: item.name }))
export const compareLinks: NavLink[] = compares.map((item) => ({ href: `/compare/${item.slug}/`, label: item.label }))

/** The header's plain links, after the Features menu. */
export const topLinks: NavLink[] = [
  { href: "/compare/", label: "Compare" },
  { href: "/pricing/", label: "Pricing" },
  { href: "/security/", label: "Security" },
  { href: "/docs/", label: "Docs" },
]

/** The comparisons the footer lists by name; the rest are one click away on /compare. */
const footerCompares = ["tailscale", "ngrok", "cloudflare-tunnel", "teleport", "netbird"]

export const footerColumns: { title: string; links: NavLink[] }[] = [
  {
    title: "Product",
    links: [
      { href: "/features/", label: "Features" },
      { href: "/pricing/", label: "Pricing" },
      { href: "/security/", label: "Security" },
      { href: "/docs/", label: "Docs" },
      { href: sourceUrl, label: "Source code" },
      { href: consoleUrl, label: "Sign in" },
    ],
  },
  { title: "Features", links: featureLinks },
  {
    title: "Compare",
    links: [
      ...compareLinks.filter((link) => footerCompares.some((slug) => link.href === `/compare/${slug}/`)),
      { href: "/compare/", label: "All comparisons" },
    ],
  },
  { title: "Use cases", links: useCaseLinks },
]

/** The legal pages, in the footer's bottom row. */
export const legalLinks: NavLink[] = legalPages.map((page) => ({ href: `/legal/${page.slug}/`, label: page.short }))
