import {
  ChartBarIcon,
  ClockCounterClockwiseIcon,
  CreditCardIcon,
  GaugeIcon,
  HardDrivesIcon,
  RocketLaunchIcon,
  LaptopIcon,
  LifebuoyIcon,
  BugIcon,
  UserIcon,
  UserGearIcon,
  UsersThreeIcon,
} from "@phosphor-icons/react/dist/ssr"
import type { Icon } from "@phosphor-icons/react/lib"
import type { Operator } from "@/lib/api"

export type Section = { href: `/${string}`; title: string; icon: Icon; roles: Operator["role"][] }

const all: Operator["role"][] = ["admin", "support", "billing", "operations"]

/** The server decides every read; the roles here only hide pages that role would be refused (403). */
export const sections: Section[] = [
  { href: "/overview", title: "Overview", icon: ChartBarIcon, roles: ["admin"] },
  { href: "/users", title: "Users", icon: UserIcon, roles: ["admin", "support", "billing"] },
  { href: "/teams", title: "Teams", icon: UsersThreeIcon, roles: ["admin", "support", "billing"] },
  { href: "/machines", title: "Machines", icon: LaptopIcon, roles: all },
  { href: "/support", title: "Support lookup", icon: LifebuoyIcon, roles: ["admin", "support", "billing"] },
  { href: "/sentry", title: "Sentry issues", icon: BugIcon, roles: ["admin", "support", "billing"] },
  { href: "/capacity", title: "Capacity", icon: GaugeIcon, roles: ["admin", "operations"] },
  { href: "/infrastructure", title: "Relays & edges", icon: HardDrivesIcon, roles: ["admin", "operations"] },
  { href: "/billing", title: "Billing & plans", icon: CreditCardIcon, roles: ["admin", "support", "billing"] },
  { href: "/releases", title: "Releases", icon: RocketLaunchIcon, roles: ["admin"] },
  { href: "/operators", title: "Operators", icon: UserGearIcon, roles: ["admin"] },
  { href: "/audit", title: "Audit log", icon: ClockCounterClockwiseIcon, roles: ["admin"] },
]

export const sectionsFor = (role: Operator["role"]) => sections.filter((s) => s.roles.includes(role))
