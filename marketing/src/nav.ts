import { consoleUrl } from "./site"
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

export const footerColumns: { title: string; links: NavLink[] }[] = [
  { title: "Features", links: [{ href: "/features/", label: "All features" }, ...featureLinks] },
  { title: "Use cases", links: useCaseLinks },
  { title: "Compare", links: [{ href: "/compare/", label: "All comparisons" }, ...compareLinks] },
  {
    title: "Paperboat",
    links: [
      { href: "/pricing/", label: "Pricing" },
      { href: "/security/", label: "Security" },
      { href: "/docs/", label: "Docs" },
      { href: "/#install", label: "Install" },
      { href: consoleUrl, label: "Sign in" },
      ...legalPages.map((page) => ({ href: `/legal/${page.slug}/`, label: page.title })),
    ],
  },
]
