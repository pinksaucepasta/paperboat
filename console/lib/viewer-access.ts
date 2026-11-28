/*
 * Who can open a terminal, a private preview or an HTTP tunnel route in a browser: the owner, plus an allow list of email
 * addresses and "*@domain" entries (the server's viewer policy). The server checks every entry again.
 */

/** The server's bound on one list. */
export const maxViewers = 100

/** What a save the server refuses as `edge_policy_transition_required` says instead of the server's sentence. */
export const edgePolicyTransitionMessage =
  "A public preview takes a list of emails or domains as it runs; to make it private or team-only, publish it again."

/** The sentence for a save refused as an unsupported edge transition, or null for any other error. */
export const viewerSaveMessage = (error: unknown): string | null =>
  (error as { code?: unknown } | null)?.code === "edge_policy_transition_required" ? edgePolicyTransitionMessage : null

/** The parts of a viewer policy the console reads. */
export type ViewerPolicyView = {
  configured: boolean
  audience?: "owner" | "team" | "specific_emails" | "email_domain" | "public"
  allowed_emails?: string[] | null
  email_domain?: string
  native_access_mode: "public" | "private" | "team"
}

// One "@", no spaces or list separators, and "*" only as the whole name before it. A domain has a dot. The one
// check for terminal sharing, previews and tunnels (components/access-list.tsx); the server checks again.
const address = /^[^\s@*,;<>()"]+@[^\s@*,;<>()"/:.]+(\.[^\s@*,;<>()"/:.]+)+$/
const wildcard = /^\*@[^\s@*,;<>()"/:.]+(\.[^\s@*,;<>()"/:.]+)+$/

export const isViewerEntry = (entry: string) =>
  entry.length <= 254 && (entry.startsWith("*") ? wildcard.test(entry) : address.test(entry))

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
