import { formatPlatform } from "@/lib/format"
import { machineOwner, type Machine, type Me } from "@/lib/api"

/**
 * The machines search box is the one source of truth for filtering, as in Tailscale: typed
 * `key:value` tokens filter, everything else is free text, and the Filter menu and its chips
 * only add, change and remove tokens in that text.
 */
export const filterKeys = {
  /** Revoked machines are hidden unless this asks for them (`isLive` below). */
  status: {
    label: "Status",
    options: { online: "Online", offline: "Offline", revoked: "Revoked" },
    test: (machine: Machine, value: string) =>
      value === "revoked" ? machine.state === "revoked" : isLive(machine) && machine.online === (value === "online"),
  },
  os: {
    label: "OS",
    options: { macos: "macOS", linux: "Linux", windows: "Windows" },
    test: (machine: Machine, value: string) => platformToken(machine.platform) === value,
  },
  /** Options come from the machines' tags (`tagOptions`); any valid tag name can be typed. */
  tag: {
    label: "Tag",
    options: {} as Record<string, string>,
    test: (machine: Machine, value: string) => (machine.tags ?? []).includes(`tag:${value}`),
  },
  owner: {
    label: "Owner",
    options: { me: "You", shared: "Shared with you" },
    test: (machine: Machine, value: string, me?: Me) =>
      machineOwner(machine, me) === (value === "me" ? "you" : "shared"),
  },
} as const

/** A machine that can still be used: not revoked. Lists show only these unless asked for revoked ones. */
export const isLive = (machine: Machine) => machine.state !== "revoked"

export type FilterKey = keyof typeof filterKeys
export type Filters = Partial<Record<FilterKey, string>>

/** The `os:` filter value for a machine platform (darwin → macos). */
export function platformToken(platform: string | undefined): string | undefined {
  if (!platform) return
  return platform === "darwin" ? "macos" : platform.toLowerCase()
}

function asFilter(token: string): [FilterKey, string] | undefined {
  const match = /^([a-z]+):(\S+)$/i.exec(token)
  if (!match) return
  const key = match[1].toLowerCase() as FilterKey
  const value = match[2].toLowerCase()
  const spec = filterKeys[key]
  if (key === "tag" && /^[a-z0-9][a-z0-9._-]{0,62}$/.test(value)) return [key, value]
  if (spec && value in spec.options) return [key, value]
}

/** The Tag filter's options: every tag on these machines, without the `tag:` prefix. */
export function tagOptions(machines: Machine[] | undefined): Record<string, string> {
  const names = new Set((machines ?? []).flatMap((machine) => machine.tags ?? []).map((tag) => tag.replace(/^tag:/, "")))
  return Object.fromEntries([...names].sort().map((name) => [name, name]))
}

/** Splits the search text into filters (last token of a key wins) and the remaining words. */
export function parseQuery(query: string): { filters: Filters; text: string } {
  const filters: Filters = {}
  const words: string[] = []
  for (const token of query.split(/\s+/).filter(Boolean)) {
    const filter = asFilter(token)
    if (filter) filters[filter[0]] = filter[1]
    else words.push(token)
  }
  return { filters, text: words.join(" ").toLowerCase() }
}

/** Sets (or, with no value, removes) one key's token, leaving the rest of the text as typed. */
export function withFilter(query: string, key: FilterKey, value?: string): string {
  const kept = query.split(/\s+/).filter((token) => token && asFilter(token)?.[0] !== key)
  if (value) kept.push(`${key}:${value}`)
  return kept.join(" ")
}

export function matchesQuery(machine: Machine, { filters, text }: ReturnType<typeof parseQuery>, me?: Me): boolean {
  if (!filters.status && !isLive(machine)) return false
  for (const [key, value] of Object.entries(filters) as [FilterKey, string][]) {
    if (!filterKeys[key].test(machine, value, me)) return false
  }
  if (!text) return true
  return [machine.alias, machine.description, machine.assigned_ip, formatPlatform(machine.platform), machine.id]
    .filter(Boolean)
    .some((field) => field!.toLowerCase().includes(text))
}
