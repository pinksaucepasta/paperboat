import { ApiError } from "@/lib/api"
import type { TerminalPhase } from "@/lib/terminal/use-terminal-connection"

/**
 * Why a browser terminal stopped, and what the page does about it, keyed by the `code` of the
 * WASM worker's close-state event (cli/cmd/browserterminal-wasm). The worker maps the socket's
 * close reason, then its close number, to one of these; anything unknown is `protocol`. The raw
 * close number is for diagnostics only and is never branched on.
 *
 * `reconnect`: the controller retries on its own. `silent`: it does so with no error shown.
 */
export type CloseCode =
  | "no_access"
  | "ended"
  | "revoked"
  | "expired"
  | "limit"
  | "bandwidth_quota_exhausted"
  | "too_many_pending"
  | "machine_unavailable"
  | "edge_unavailable"
  | "terminal_service_unavailable"
  | "protocol"

export type CloseOutcome = { phase: TerminalPhase | "reconnect"; message: string; silent?: boolean }

export const terminalCloseCodes: Readonly<Record<CloseCode, CloseOutcome>> = {
  no_access: { phase: "denied", message: "You do not have access to this terminal. Ask its owner." },
  // Revoked only says the access policy changed: a fresh ticket says whether this person is still allowed
  // (the owner always is), so the page asks instead of showing "removed".
  revoked: { phase: "reconnect", message: "", silent: true },
  ended: { phase: "closed", message: "This terminal has ended." },
  expired: { phase: "reconnect", message: "", silent: true },
  limit: { phase: "limit", message: "Your plan's browser terminal limit is reached. Close one or change plan." },
  bandwidth_quota_exhausted: {
    phase: "quota",
    message: "This workspace's hosted bandwidth is used up. Add bandwidth or change plan under Billing.",
  },
  too_many_pending: { phase: "reconnect", message: "Too many terminals are opening at once. Waiting as the server asks." },
  machine_unavailable: {
    phase: "reconnect",
    message: "The machine is unavailable right now. This resumes when it is back.",
  },
  edge_unavailable: { phase: "reconnect", message: "The connection was interrupted." },
  terminal_service_unavailable: {
    phase: "reconnect",
    message: "The machine's terminal service is not responding. This resumes when it answers.",
  },
  protocol: { phase: "transport-error", message: "The machine sent something this page could not use. You can retry." },
}

function isCloseCode(code: string | undefined): code is CloseCode {
  return !!code && Object.hasOwn(terminalCloseCodes, code)
}

/**
 * Failures inside the encrypted channel (the machine's attach and stream errors, not socket
 * closes) carry the host protocol's codes; each names one of the table's codes.
 */
const workerCodes: Readonly<Record<string, CloseCode>> = {
  connection_failed: "edge_unavailable",
  connection_lost: "edge_unavailable",
  output_key_lag: "edge_unavailable",
  replay_gap: "edge_unavailable",
  credential_expired: "expired",
  not_found_or_forbidden: "no_access",
  stale_generation: "revoked",
  machine_identity_unverified: "no_access",
  terminal_closed: "ended",
  closed: "ended",
}

/** The code of a worker `state` event: a socket close's code, else the in-channel code's. */
export function workerCloseCode(event: { state?: string; code?: string }): CloseCode | null {
  if (isCloseCode(event.code)) return event.code
  if (event.code && Object.hasOwn(workerCodes, event.code)) return workerCodes[event.code]
  if (event.state === "closed") return "ended"
  if (event.state === "denied") return "no_access"
  if (event.state === "transport-error") return "protocol"
  return null
}

/** Ticket API refusals, by the server's error code first and its status second. */
const ticketCodes: Readonly<Record<string, CloseCode>> = {
  machine_edge_unavailable: "machine_unavailable",
  terminal_session_closed: "ended",
  too_many_pending_tickets: "too_many_pending",
  browser_terminal_limit_reached: "limit",
  bandwidth_quota_exhausted: "bandwidth_quota_exhausted",
}

/** The code of a failed ticket request; null when it was not an API refusal. */
export function ticketCloseCode(reason: unknown): CloseCode | null {
  if (!(reason instanceof ApiError)) return null
  if (Object.hasOwn(ticketCodes, reason.code)) return ticketCodes[reason.code]
  if (reason.status === 401 || reason.status === 403) return "no_access"
  // A missing terminal has ended; it is not an access problem.
  if (reason.status === 404) return "ended"
  if (reason.status === 429) return "too_many_pending"
  return "edge_unavailable"
}
