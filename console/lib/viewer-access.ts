/*
 * Who can open a private preview or an HTTP tunnel route in a browser: the owner, plus an allow list of email
 * addresses and "*@domain" entries (the server's viewer policy). The server checks every entry again.
 */

/** The server's bound on one list. */
export const maxViewers = 100

/** The parts of a viewer policy the console reads. */
export type ViewerPolicyView = {
  configured: boolean
  audience?: "owner" | "team" | "specific_emails" | "email_domain" | "public"
  allowed_emails?: string[] | null
  email_domain?: string
  native_access_mode: "public" | "private" | "team"
}

// One "@", no spaces or list separators, and "*" only as the whole name before it. A wildcard's domain has a dot.
const address = /^[^\s@*,;<>()"]+@[^\s@*,;<>()"/:]+$/
const wildcard = /^\*@[^\s@*,;<>()"/:.]+(\.[^\s@*,;<>()"/:.]+)+$/

export const isViewerEntry = (entry: string) =>
  entry.length <= 254 && (entry.startsWith("*") ? wildcard.test(entry) : address.test(entry))

/** The list with new entries added, lower-cased as the server stores them, without repeats. */
export const addViewers = (list: string[], entries: string[]) => [
  ...new Set([...list, ...entries.map((entry) => entry.toLowerCase())]),
]

/** Why this list cannot be saved, or null. */
export function viewerListProblem(list: string[]): string | null {
  const wrong = list.filter((entry) => !isViewerEntry(entry))
  if (wrong.length) return `Not an email address or *@domain: ${wrong.join(", ")}`
  if (list.length > maxViewers) return `A list holds at most ${maxViewers} entries.`
  return null
}

/** The saved list. A one-domain policy reads as its "*@domain" entry. */
export function savedViewers(policy: ViewerPolicyView): string[] {
  if (!policy.configured) return []
  if (policy.audience === "specific_emails") return policy.allowed_emails ?? []
  if (policy.audience === "email_domain" && policy.email_domain) return [`*@${policy.email_domain}`]
  return []
}

/** The policy that saves this list: an empty one leaves only the owner. Nothing here makes a resource public. */
export const viewerMutation = (list: string[]) =>
  list.length
    ? { audience: "specific_emails" as const, allowed_emails: list }
    : { audience: "owner" as const }

/** A public tunnel route that a saved policy still limits to some people. */
export const isLimitedPublicRoute = (policy: ViewerPolicyView) =>
  policy.native_access_mode === "public" && policy.configured && policy.audience !== "public"
