import type { BrowserTerminalAPI } from "@/lib/terminal/browser-terminal-wasm"
import { verifyTicketKey, type RootPin } from "@/lib/terminal/browser-root-key"

export type TerminalRole = "owner" | "viewer" | "interactive"

/** The server's answer to `POST terminal-sessions/{id}/browser-ticket`. */
export type Ticket = {
  stream_url: string
  edge_node_id: string
  subprotocols: string[]
  expires_at: string
  role: TerminalRole
  machine_id: string
  owner_account_id: string
  root_key_id: string
  root_public_key: string
  root_fingerprint: string
}

/**
 * Checks a browser ticket against the terminal it was asked for and the machine's pinned key, and builds the
 * WASM worker's `connect` config. Shared by the terminal page and the Files page's Inbox uploads.
 */
export async function ticketConnectConfig(
  browser: BrowserTerminalAPI,
  ticket: Ticket,
  session: { id: string; ownerAccount: string; machineId: string },
  fromSequence = "0",
): Promise<{ config: Record<string, unknown>; pin: RootPin }> {
  if (ticket.owner_account_id !== session.ownerAccount || ticket.machine_id !== session.machineId)
    throw new Error("The ticket did not match this terminal's owner and machine.")
  if (!/^[a-zA-Z0-9][a-zA-Z0-9._-]{2,127}$/.test(ticket.edge_node_id))
    throw new Error("The terminal ticket has no valid edge identity.")
  const pin = await verifyTicketKey(
    browser,
    ticket.owner_account_id,
    ticket.root_public_key,
    ticket.root_fingerprint,
    ticket.root_key_id,
  )
  if (ticket.role !== "owner" && ticket.role !== "viewer" && ticket.role !== "interactive")
    throw new Error("The ticket has an invalid access role.")
  if (
    !Array.isArray(ticket.subprotocols) ||
    ticket.subprotocols.length !== 2 ||
    ticket.subprotocols[0] !== "paperboat.browser-terminal.e2ee.v1" ||
    !ticket.subprotocols[1].startsWith("pb-ticket.")
  )
    throw new Error("The encrypted terminal ticket is invalid.")
  const expiresAt = Date.parse(ticket.expires_at)
  if (!Number.isFinite(expiresAt) || expiresAt <= Date.now())
    throw new Error("The ticket expired before connecting. Retry for a new one.")
  const streamURL = new URL(ticket.stream_url)
  if (streamURL.protocol !== "wss:" || streamURL.username || streamURL.password || streamURL.hash)
    throw new Error("The terminal service did not provide a secure address.")
  return {
    pin,
    config: {
      url: streamURL.href,
      subprotocols: ticket.subprotocols,
      owner_account_id: session.ownerAccount,
      machine_id: session.machineId,
      root_key_id: pin.rootKeyID,
      root_public_key: pin.publicKey,
      root_fingerprint: pin.fingerprint,
      terminal_session_id: session.id,
      role: ticket.role,
      from_sequence: fromSequence,
      at_live_boundary: true,
    },
  }
}
