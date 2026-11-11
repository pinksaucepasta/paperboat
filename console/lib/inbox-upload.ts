import { api, ApiError } from "@/lib/api"
import { terminalCloseCodes, ticketCloseCode, workerCloseCode } from "@/lib/terminal/close-codes"
import { loadBrowserTerminal, type BrowserTerminalEvent } from "@/lib/terminal/browser-terminal-wasm"
import { ticketConnectConfig, type Ticket } from "@/lib/terminal/browser-ticket"
import { FilePasteError, FilePasteUploader, filePasteMessage, type FilePasteProgress } from "@/lib/terminal/file-paste"
import { closeTerminal, startShell } from "@/lib/terminal/queries"
import type { TerminalSession } from "@/lib/terminal/use-terminal-connection"

/** A failure to put it in words: the terminal's own messages for its limits, closes and file errors. */
function describe(reason: unknown): string {
  if (reason instanceof FilePasteError) return filePasteMessage(reason.code)
  const closed = ticketCloseCode(reason)
  if (closed && reason instanceof ApiError && closed !== "edge_unavailable") return terminalCloseCodes[closed].message
  return reason instanceof Error && reason.message ? reason.message : "The machine could not be reached."
}

/**
 * Puts files in a machine's Paperboat Inbox the way the web terminal's upload does: over the terminal's
 * end-to-end encrypted connection (browser-terminal WASM), through the same chunked, hashed transfer
 * (`FilePasteUploader`). It opens a shell session only to carry that connection: no terminal is shown
 * and nothing is typed into it; the session is closed when the upload ends or is canceled. Counts toward
 * the plan's browser-terminal limit while it runs. Rejects with a sentence that is safe to show.
 */
export function uploadToInbox(
  machine: { id: string },
  workspace: string,
  files: File[],
  onProgress: (progress: FilePasteProgress) => void,
): { done: Promise<void>; cancel: () => void } {
  let canceled = false
  let stop: () => unknown = () => undefined
  const done = (async () => {
    let sessionID = ""
    let connectionID = ""
    let browser: Awaited<ReturnType<typeof loadBrowserTerminal>> | undefined
    let identityID = ""
    try {
      browser = await loadBrowserTerminal()
      sessionID = (await startShell(machine.id, workspace)).id
      if (canceled) throw new FilePasteError("canceled")
      const identity = await browser.createIdentity()
      if ("error" in identity) throw new Error(identity.message)
      identityID = identity.id
      const ticket = await api<Ticket>(`terminal-sessions/${encodeURIComponent(sessionID)}/browser-ticket`, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify({ browser_public_key_sha256: identity.public_key_sha256 }),
        cache: "no-store",
      })
      // The ticket must be for this machine and for the account that owns the new shell, as the terminal page checks.
      const owner = await api<TerminalSession>(`terminal-sessions/${encodeURIComponent(sessionID)}/sharing`)
      const { config, pin } = await ticketConnectConfig(browser, ticket, {
        id: sessionID,
        ownerAccount: owner.owner_account,
        machineId: machine.id,
      })
      const runtime = browser
      const attached = new Promise<void>((resolve, reject) => {
        const timer = setTimeout(() => reject(new Error("The machine did not answer in time.")), 30_000)
        const end = (action: () => void) => {
          clearTimeout(timer)
          action()
        }
        const onEvent = (event: BrowserTerminalEvent) => {
          if (event.type === "identity-verified" && (event.machine_id !== machine.id || event.root_key_id !== pin.rootKeyID))
            end(() => reject(new Error("The machine's identity did not match.")))
          else if (event.type === "attached") end(resolve)
          else if (event.type === "state" && ["reconnecting", "denied", "closed", "transport-error"].includes(event.state ?? "")) {
            const outcome = terminalCloseCodes[workerCloseCode(event) ?? "edge_unavailable"]
            end(() => reject(new Error(outcome.message || "The connection to the machine was interrupted.")))
          }
        }
        void runtime.connect(identityID, config, onEvent).then(
          (result) => {
            identityID = ""
            if (!result.id) return end(() => reject(new Error(result.message || "The encrypted connection could not be started.")))
            connectionID = result.id
          },
          (reason) => end(() => reject(reason)),
        )
      })
      await attached
      if (canceled) throw new FilePasteError("canceled")
      const uploader = new FilePasteUploader(() => (connectionID ? { api: runtime, connectionID } : null), (progress) => progress && onProgress(progress))
      stop = () => uploader.cancel()
      await uploader.upload(files)
    } catch (reason) {
      throw new Error(canceled ? "Upload canceled." : describe(reason))
    } finally {
      if (identityID) browser?.discardIdentity(identityID)
      if (connectionID) browser?.leave(connectionID)
      if (sessionID) void closeTerminal(machine.id, sessionID).catch(() => undefined)
    }
  })()
  return {
    done,
    cancel: () => {
      canceled = true
      void stop()
    },
  }
}
