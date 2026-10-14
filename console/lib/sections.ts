import {
  ChartBarIcon,
  ClockCounterClockwiseIcon,
  EyeIcon,
  FolderIcon,
  GlobeIcon,
  KeyIcon,
  LaptopIcon,
  PlugsIcon,
  ShareNetworkIcon,
  SwapIcon,
  TerminalWindowIcon,
} from "@phosphor-icons/react/dist/ssr"
import type { Icon } from "@phosphor-icons/react/lib"

export type Section = {
  href: "/" | `/${string}`
  title: string
  icon: Icon
  /** Sections whose content belongs to individual machines get a machine picker. */
  machineScoped: boolean
}

export const machinesSection: Section = { href: "/", title: "Machines", icon: LaptopIcon, machineScoped: false }

export const sections = {
  terminals: { href: "/terminals", title: "Terminals", icon: TerminalWindowIcon, machineScoped: true },
  files: { href: "/files", title: "Files", icon: FolderIcon, machineScoped: true },
  previews: { href: "/previews", title: "Previews", icon: EyeIcon, machineScoped: true },
  tunnels: { href: "/tunnels", title: "Tunnels", icon: PlugsIcon, machineScoped: true },
  domains: { href: "/domains", title: "Domains", icon: GlobeIcon, machineScoped: false },
  environment: { href: "/environment", title: "Environment", icon: KeyIcon, machineScoped: true },
  config: { href: "/config", title: "Config sync", icon: SwapIcon, machineScoped: true },
  network: { href: "/network", title: "Network", icon: ShareNetworkIcon, machineScoped: false },
  usage: { href: "/usage", title: "Usage", icon: ChartBarIcon, machineScoped: true },
  activity: { href: "/activity", title: "Activity", icon: ClockCounterClockwiseIcon, machineScoped: true },
} satisfies Record<string, Section>

/**
 * Sidebar order. Machines is home and stands alone. Connect holds the daily ways into a machine, most
 * used first (Terminals lead as the hero); things set up once go under Configure, things you check under Monitor.
 */
export const navigationGroups: { label?: string; items: Section[] }[] = [
  { items: [machinesSection] },
  { label: "Connect", items: [sections.terminals, sections.previews, sections.tunnels, sections.files] },
  { label: "Configure", items: [sections.environment, sections.config, sections.domains, sections.network] },
  { label: "Monitor", items: [sections.usage, sections.activity] },
]

/** Machine tabs, in the sidebar's order. Each is a route under /machines/<id>, so the list can grow without crowding the page. */
export const machineTabs: { slug: string; title: string }[] = [
  { slug: "", title: "Overview" },
  { slug: "terminals", title: "Terminals" },
  { slug: "previews", title: "Previews" },
  { slug: "tunnels", title: "Tunnels" },
  { slug: "files", title: "Files" },
  { slug: "environment", title: "Environment" },
  { slug: "config", title: "Config sync" },
  { slug: "usage", title: "Usage" },
  { slug: "activity", title: "Activity" },
]
