import { ApiError, type Data } from "@/lib/api"
import type { RootPin } from "@/lib/terminal/browser-root-key"
import type { BrowserTerminalAPI } from "@/lib/terminal/browser-terminal-wasm"
import type { FilePasteProgress } from "@/lib/terminal/file-paste"
import type { MobileModifiers } from "@/lib/terminal/mobile-keys"

/** The browser terminal connection's types, ticket checks and error wording. */
export type TerminalSession = Data<"/v1/terminal-sessions/{session_id}/sharing">
export type TerminalRole = "owner" | "viewer" | "interactive"
export type TerminalPhase =
  | "loading"
  | "connecting"
  | "attached"
  | "reconnecting"
  | "denied"
  | "closed"
  | "transport-error"
  /** The workspace's hosted bandwidth is used up: retrying can't help until it renews or is added to. */
  | "quota"
  /** The plan's browser-terminal limit is reached: `limit` holds the server's refusal. */
  | "limit"

/** One attachment as the host reports it: identities only; names come from /sharing. */
export type HostParticipant = {
  attachment_id: string
  account_id: string
  client_id: string
  role: "owner" | "viewer" | "interactive"
  connected_at: string
}

export type TerminalAccessMethod = "account" | "code"

export type TerminalTicket = {
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

/** Accepted close requests cannot be reopened before the runtime confirms them. */
export function terminalCanAttach(session: Pick<TerminalSession, "state" | "desired_state">): boolean {
  // `browser-session` (guests) reports the raw runtime state "running"; the other endpoints say
  // attached or detached for the same thing.
  return session.desired_state === "open" && ["attached", "detached", "running", "unavailable"].includes(session.state)
}

/** What the renderer gives the controller: the VT screen and its input element. */
export type TerminalScreen = {
  write(data: Uint8Array | string): void
  resize(columns: number, rows: number): void
  textarea(): HTMLTextAreaElement | null
  cursorKeysApp(): boolean
  /** Sizes the screen to its host; a size change comes back through `screenResized`. */
  fit(): void
}

/** The browser and network the controller drives; tests pass fakes. */
export type ControllerDeps = {
  loadBrowserTerminal(): Promise<BrowserTerminalAPI>
  requestTicket(
    accessMethod: TerminalAccessMethod,
    sessionID: string,
    browserKeySHA256: string,
    signal: AbortSignal,
  ): Promise<TerminalTicket>
  verifyTicketKey(
    api: BrowserTerminalAPI,
    ownerAccountID: string,
    publicKey: string,
    fingerprint: string,
    keyID: string,
  ): Promise<RootPin>
  setTimeout(callback: () => void, ms: number): number
  clearTimeout(id: number | undefined): void
  requestAnimationFrame(callback: () => void): void
  hidden(): boolean
  now(): number
}

export type TerminalSnapshot = {
  readonly phase: TerminalPhase
  readonly role: TerminalRole
  readonly catchingUp: boolean
  readonly error: string
  readonly notice: string
  readonly connectedEdge: string
  /** The window title the shell sets (OSC 0/2), seen as it happens; the server's copy lags. */
  readonly title: string
  readonly limit: ApiError | null
  /** Who is attached, pushed by the host (newer hosts only); null until the first such event. */
  readonly participants: HostParticipant[] | null
  readonly mobileModifiers: MobileModifiers
  readonly uploadProgress: FilePasteProgress | null
  readonly uploadNotice: string
  readonly uploadBusy: boolean
  readonly dragOver: boolean
  /** Document visibility as the controller last saw it. */
  readonly visibility: "visible" | "hidden"
  /** Whether this attachment sends the machine's terminal its size. */
  readonly resizeControl: boolean
  /** Reconnects scheduled in this page's life. */
  readonly reconnects: number
}

export function apiErrorMessage(error: unknown): string {
  if (error instanceof ApiError) {
    if (error.status === 401) return "Your sign-in expired. Sign in again before opening this terminal."
    if (error.code === "terminal_session_closed")
      return "This terminal is closing, closed, or failed. Check its details before retrying."
    if (error.status === 403 || error.status === 404)
      return "This terminal is unavailable or your access has changed. Ask the owner."
    return "The terminal service could not be reached. You can retry safely."
  }
  return error instanceof Error ? error.message : "The terminal could not be opened."
}

export function checkResult(value: void | { error: string; message: string }): void {
  if (value && typeof value === "object" && "error" in value) throw new Error(value.message)
}

export function pasteIntoTextarea(textarea: HTMLTextAreaElement, text: string): boolean {
  try {
    const event = new Event("paste", { bubbles: true, cancelable: true })
    Object.defineProperty(event, "clipboardData", {
      value: { getData: (format: string) => (format === "text" || format === "text/plain" ? text : "") },
    })
    textarea.dispatchEvent(event)
    return event.defaultPrevented
  } catch {
    return false
  }
}

/** Checks a ticket against the session it was issued for, before its root key is verified. */
export function checkTicketTarget(ticket: TerminalTicket, session: TerminalSession): void {
  if (ticket.owner_account_id !== session.owner_account || ticket.machine_id !== session.target.machine_id)
    throw new Error("The ticket did not match this terminal's owner and machine.")
  if (!/^[a-zA-Z0-9][a-zA-Z0-9._-]{2,127}$/.test(ticket.edge_node_id))
    throw new Error("The terminal ticket has no valid edge identity.")
}

/** Checks the ticket's role, channel and expiry; returns its validated stream URL. */
export function checkTicketChannel(ticket: TerminalTicket, now: number): URL {
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
  if (!Number.isFinite(expiresAt) || expiresAt <= now)
    throw new Error("The ticket expired before connecting. Retry for a new one.")
  const streamURL = new URL(ticket.stream_url)
  if (streamURL.protocol !== "wss:" || streamURL.username || streamURL.password || streamURL.hash)
    throw new Error("The terminal service did not provide a secure address.")
  return streamURL
}

/** The terminal's state as data attributes, so scripted checks read state instead of text. */
export function terminalDataAttributes(
  state: Pick<TerminalSnapshot, "phase" | "role" | "catchingUp" | "visibility" | "resizeControl" | "reconnects">,
): Record<`data-terminal-${string}`, string> {
  return {
    "data-terminal-phase": state.phase,
    "data-terminal-role": state.role,
    "data-terminal-catching-up": String(state.catchingUp),
    "data-terminal-visibility": state.visibility,
    "data-terminal-resize-control": String(state.resizeControl),
    "data-terminal-reconnects": String(state.reconnects),
  }
}
