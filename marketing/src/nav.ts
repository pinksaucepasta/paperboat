import { consoleUrl } from "./site"
import { compares } from "./compare-data"

/** The site's navigation, in one place: the header and the footer both read it. */
export type NavLink = { href: string; label: string }
export type NavGroup = { title: string; href?: string; links: NavLink[] }

export const navGroups: NavGroup[] = [
  {
    title: "Product",
    links: [
      { href: "/#terminals", label: "Terminals" },
      { href: "/#links", label: "Previews and tunnels" },
      { href: "/#teams", label: "Teams" },
      { href: "/#network", label: "Network" },
    ],
  },
  {
    title: "Compare",
    links: compares.map((compare) => ({ href: `/compare/${compare.slug}`, label: `Paperboat vs ${compare.name}` })),
  },
  { title: "Pricing", href: "/pricing", links: [] },
  { title: "Docs", href: "/docs", links: [] },
]

export const getStartedLinks: NavLink[] = [
  { href: "/#install", label: "Install" },
  { href: consoleUrl, label: "Console" },
]
