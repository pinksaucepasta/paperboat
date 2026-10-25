/**
 * The machines search box is the one source of truth for filtering, as in Tailscale: typed
 * `key:value` tokens filter, everything else is free text, and the Filter menu and its chips
 * only add, change and remove tokens in that text.
 */
export const filterKeys = {
  /** The server leaves revoked machines out unless this asks for them. */
  status: {
    label: "Status",
    options: { online: "Online", offline: "Offline", revoked: "Revoked" },
  },
  os: {
    label: "OS",
    options: { macos: "macOS", linux: "Linux", windows: "Windows" },
  },
  /** Options come from the workspace's tags (GET .../tags); any valid tag name can be typed. */
  tag: {
    label: "Tag",
    options: {} as Record<string, string>,
  },
  owner: {
    label: "Owner",
    options: { me: "You", shared: "Shared with you" },
  },
} as const

export type FilterKey = keyof typeof filterKeys
export type Filters = Partial<Record<FilterKey, string>>

function asFilter(token: string): [FilterKey, string] | undefined {
  const match = /^([a-z]+):(\S+)$/i.exec(token)
  if (!match) return
  const key = match[1].toLowerCase() as FilterKey
  const value = match[2].toLowerCase()
  const spec = filterKeys[key]
  if (key === "tag" && /^[a-z0-9][a-z0-9._-]{0,62}$/.test(value)) return [key, value]
  if (spec && value in spec.options) return [key, value]
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
