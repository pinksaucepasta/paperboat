import { api, ApiError } from "@/lib/api"
import { verifyTicketKey, type RootPin } from "@/lib/terminal/browser-root-key"
import { loadBrowserTerminal, type BrowserTerminalAPI, type BrowserTerminalEvent } from "@/lib/terminal/browser-terminal-wasm"
import { terminalCloseCodes, ticketCloseCode, workerCloseCode } from "@/lib/terminal/close-codes"
import { FilePasteError, FilePasteUploader, filePasteMessage, type FilePasteProgress } from "@/lib/terminal/file-paste"
import { closeTerminal, startShell } from "@/lib/terminal/queries"
import type { TerminalSession } from "@/lib/terminal/use-terminal-connection"

/**
 * Everything about putting files in a machine's Paperboat Inbox over the terminal's end-to-end encrypted
 * browser connection: opening that connection (`openBrowserConnection`, which the terminal page uses for its
 * own shell too) and the one-shot upload the Files page uses (`uploadToInbox`). The transfer itself, chunked
 * and hashed, is `FilePasteUploader`; the terminal adds only the typing of the resulting paths.
 */

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

export type ConnectionSession = { id: string; ownerAccount: string; machineId: string }

/** Checks a ticket against the terminal it was asked for and the machine's pinned key; builds the worker's connect config. */
async function ticketConnectConfig(
  browser: BrowserTerminalAPI,
  ticket: Ticket,
  session: ConnectionSession,
  fromSequence: string,
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

/**
 * Opens one encrypted connection to a terminal session: a fresh browser key, the server's ticket for it,
 * the checks above, then the worker's connect. Events go to `onEvent` with the connection's id. Resolves
 * null, with the key discarded and any connection left, when `canceled` turns true along the way.
 */
export async function openBrowserConnection(
  browser: BrowserTerminalAPI,
  options: {
    session: ConnectionSession
    fetchTicket: (browserKeySha256: string) => Promise<Ticket>
    onEvent: (connectionID: string, pin: RootPin, event: BrowserTerminalEvent) => void
    /** Called with the verified ticket just before connecting. */
    onTicket?: (ticket: Ticket) => void
    fromSequence?: string
    canceled?: () => boolean
  },
): Promise<{ id: string; ticket: Ticket } | null> {
  const canceled = options.canceled ?? (() => false)
  const identity = await browser.createIdentity()
  if ("error" in identity) throw new Error(identity.message)
  let identityID = identity.id
  try {
    if (canceled()) return null
    const ticket = await options.fetchTicket(identity.public_key_sha256)
    if (canceled()) return null
    const { config, pin } = await ticketConnectConfig(browser, ticket, options.session, options.fromSequence ?? "0")
    options.onTicket?.(ticket)
    let startedID = ""
    const started = await browser.connect(identityID, config, (event) => options.onEvent(startedID, pin, event))
    if (!started.id) throw new Error(started.message || "The encrypted connection could not be started.")
    startedID = started.id
    identityID = ""
    if (canceled()) {
      browser.leave(startedID)
      return null
    }
    return { id: startedID, ticket }
  } finally {
    if (identityID) browser.discardIdentity(identityID)
  }
}

/** A failure in words: the terminal's own messages for its limits and closes, and for file errors. */
export function describeUploadFailure(reason: unknown): string {
  if (reason instanceof FilePasteError) return filePasteMessage(reason.code)
  const closed = ticketCloseCode(reason)
  if (closed && reason instanceof ApiError) return terminalCloseCodes[closed].message || "The machine could not be reached."
  return reason instanceof Error && reason.message ? reason.message : "The machine could not be reached."
}

/** The machine ended the connection before the upload could start. */
class ConnectionClosed extends Error {
  constructor(
    message: string,
    readonly transient: boolean,
  ) {
    super(message)
  }
}

/** Closes that heal on their own (a machine still ending its last shell, an edge restarting), which the terminal page reconnects through. */
const retryDelays = [2000, 5000, 10_000]
const isTransient = (reason: unknown) => {
  if (reason instanceof ConnectionClosed) return reason.transient
  const closed = reason instanceof ApiError ? ticketCloseCode(reason) : null
  return closed !== null && terminalCloseCodes[closed].phase === "reconnect"
}

/** Runs `attempt` again after each delay while it fails with a close that heals on its own; any other failure ends it. */
export async function retryTransient<T>(
  attempt: () => Promise<T>,
  options: { delays?: number[]; canceled?: () => boolean; wait?: (ms: number) => Promise<unknown> } = {},
): Promise<T> {
  const { delays = retryDelays, canceled = () => false, wait = (ms) => new Promise((resolve) => setTimeout(resolve, ms)) } = options
  for (let tried = 0; ; tried++) {
    try {
      return await attempt()
    } catch (reason) {
      if (canceled() || tried >= delays.length || !isTransient(reason)) throw reason
      await wait(delays[tried])
    }
  }
}

/**
 * Puts files in a machine's Inbox the way the terminal's upload does, with no terminal shown and nothing
 * typed: a shell session is opened only to carry the connection, and closed when the upload ends or is
 * canceled. It counts toward the plan's browser-terminal limit while it runs. `done` rejects with a
 * sentence that is safe to show.
 */
export function uploadToInbox(
  machine: { id: string },
  workspace: string,
  files: File[],
  onProgress: (progress: FilePasteProgress) => void,
): { done: Promise<void>; cancel: () => void } {
  let canceled = false
  let cancelUpload: () => unknown = () => undefined
  const connect = async (browser: BrowserTerminalAPI, sessionID: string, attached: { resolve: () => void; reject: (error: Error) => void }) => {
    // The shell's owner is who the ticket must be for, as the terminal page checks.
    const owner = await api<TerminalSession>(`terminal-sessions/${encodeURIComponent(sessionID)}/sharing`)
    return openBrowserConnection(browser, {
      session: { id: sessionID, ownerAccount: owner.owner_account, machineId: machine.id },
      canceled: () => canceled,
      fetchTicket: (key) =>
        api<Ticket>(`terminal-sessions/${encodeURIComponent(sessionID)}/browser-ticket`, {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ browser_public_key_sha256: key }),
          cache: "no-store",
        }),
      onEvent: (_, pin, event) => {
        if (event.type === "identity-verified" && (event.machine_id !== machine.id || event.root_key_id !== pin.rootKeyID))
          attached.reject(new Error("The machine's identity did not match."))
        else if (event.type === "attached") attached.resolve()
        else if (event.type === "state" && ["reconnecting", "denied", "closed", "transport-error"].includes(event.state ?? "")) {
          const outcome = terminalCloseCodes[workerCloseCode(event) ?? "edge_unavailable"]
          attached.reject(new ConnectionClosed(outcome.message || "The connection to the machine was interrupted.", outcome.phase === "reconnect"))
        }
      },
    })
  }
  const done = (async () => {
    let sessionID = ""
    let connectionID = ""
    let browser: BrowserTerminalAPI | undefined
    try {
      browser = await loadBrowserTerminal()
      sessionID = (await startShell(machine.id, workspace)).id
      await retryTransient(
        async () => {
          let ready!: { resolve: () => void; reject: (error: Error) => void }
          const attached = new Promise<void>((resolve, reject) => (ready = { resolve, reject }))
          const timer = setTimeout(() => ready.reject(new Error("The machine did not answer in time.")), 30_000)
          try {
            const opened = await connect(browser!, sessionID, ready)
            if (!opened) throw new FilePasteError("canceled")
            connectionID = opened.id
            await attached
          } catch (reason) {
            if (connectionID) browser!.leave(connectionID)
            connectionID = ""
            throw reason
          } finally {
            clearTimeout(timer)
            attached.catch(() => undefined)
          }
        },
        { canceled: () => canceled },
      )
      if (canceled) throw new FilePasteError("canceled")
      const runtime = browser
      const uploader = new FilePasteUploader(() => ({ api: runtime, connectionID }), (progress) => progress && onProgress(progress))
      cancelUpload = () => uploader.cancel()
      await uploader.upload(files)
    } catch (reason) {
      throw new Error(canceled ? "Upload canceled." : describeUploadFailure(reason))
    } finally {
      if (connectionID) browser?.leave(connectionID)
      if (sessionID) void closeTerminal(machine.id, sessionID).catch(() => undefined)
    }
  })()
  return {
    done,
    cancel: () => {
      canceled = true
      void cancelUpload()
    },
  }
}
