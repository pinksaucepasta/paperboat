import { consoleUrl } from "./site"
import { compares } from "./compare-data"
import { products } from "./product-data"
import { legalPages } from "./legal-data"

/** The site's navigation, in one place: the header and the footer both read it. */
export type NavLink = { href: string; label: string }
/** A group with an `href` is one link in the header (its own page lists the rest); the footer lists its links. */
export type NavGroup = { title: string; href?: string; links: NavLink[] }

export const navGroups: NavGroup[] = [
  {
    title: "Product",
    links: products.map((product) => ({ href: `/product/${product.slug}`, label: product.name })),
  },
  {
    title: "Compare",
    href: "/compare",
    links: compares.map((compare) => ({ href: `/compare/${compare.slug}`, label: `Paperboat vs ${compare.name}` })),
  },
  { title: "Pricing", href: "/pricing", links: [] },
  { title: "Docs", href: "/docs", links: [] },
]

export const getStartedLinks: NavLink[] = [
  { href: "/#install", label: "Install" },
  { href: consoleUrl, label: "Console" },
]

/** The footer's last column: security and the legal pages. */
export const legalLinks: NavLink[] = [
  { href: "/security", label: "Security" },
  ...legalPages.map((page) => ({ href: `/legal/${page.slug}`, label: page.title })),
]
