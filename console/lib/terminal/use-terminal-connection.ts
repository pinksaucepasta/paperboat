"use client"

import "@wterm/dom/css"

import { WTerm } from "@wterm/dom"
import { GhosttyCore } from "@wterm/ghostty"
import { useCallback, useEffect, useRef, useState } from "react"
import { api, ApiError, type Data } from "@/lib/api"
import { guestApi } from "@/lib/terminal/queries"
import { verifyTicketKey, type RootPin } from "@/lib/terminal/browser-root-key"
import {
  loadBrowserTerminal,
  type BrowserTerminalAPI,
  type BrowserTerminalEvent,
} from "@/lib/terminal/browser-terminal-wasm"
import {
  classifyFilePaste,
  FilePasteError,
  FilePasteUploader,
  filePasteMessage,
  type FilePasteProgress,
} from "@/lib/terminal/file-paste"
import {
  applyMobileModifiers,
  mobileKeySequence,
  noMobileModifiers,
  type MobileKey,
  type MobileModifiers,
} from "@/lib/terminal/mobile-keys"
import { terminalCloseCodes, ticketCloseCode, workerCloseCode } from "@/lib/terminal/close-codes"
import { paletteSequence, type TerminalTheme } from "@/lib/terminal/themes"

/**
 * The browser terminal's connection: an end-to-end encrypted channel to the machine (run by the
 * WASM worker), Ghostty's VT core and wterm's renderer. Ported from paperboat-dashboard's
 * browser console with its protocol handling unchanged; the UI is left to the caller, so the
 * same terminal can fill a page or sit in a quick panel.
 *
 * The renderer depends on the session's id only, so a caller can keep polling the session
 * (participants, title) without tearing down a live terminal.
 */

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

type Ticket = {
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

function normalizeSequence(value: string): string {
  if (!/^\d{1,20}$/.test(value)) throw new Error("The terminal returned an invalid sequence number.")
  return value.replace(/^0+(?=\d)/, "")
}

function compareSequence(left: string, right: string): number {
  const a = normalizeSequence(left)
  const b = normalizeSequence(right)
  if (a.length !== b.length) return a.length < b.length ? -1 : 1
  return a === b ? 0 : a < b ? -1 : 1
}

function subtractSequence(left: string, right: string): string {
  const a = normalizeSequence(left)
  const b = normalizeSequence(right)
  if (compareSequence(a, b) < 0) throw new Error("Terminal output sequences moved backwards.")
  let borrow = 0
  let result = ""
  for (let offset = 0; offset < a.length; offset++) {
    const aIndex = a.length - 1 - offset
    const bIndex = b.length - 1 - offset
    let digit = Number(a[aIndex]) - borrow - (bIndex >= 0 ? Number(b[bIndex]) : 0)
    borrow = digit < 0 ? 1 : 0
    if (digit < 0) digit += 10
    result = String(digit) + result
  }
  return result.replace(/^0+(?=\d)/, "")
}

function sequenceOffset(later: string, earlier: string): number {
  const difference = subtractSequence(later, earlier)
  return difference.length > 7 ? Number.MAX_SAFE_INTEGER : Number(difference)
}

function apiErrorMessage(error: unknown): string {
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

function checkResult(value: void | { error: string; message: string }): void {
  if (value && typeof value === "object" && "error" in value) throw new Error(value.message)
}

function pasteIntoWTerm(textarea: HTMLTextAreaElement, text: string): boolean {
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

export function useTerminalConnection({
  session,
  theme,
  hostRef,
  accessMethod = "account",
}: {
  session: TerminalSession
  theme: TerminalTheme
  /** "code" for guests who joined with a join code: they get a guest ticket and have no account. */
  accessMethod?: "account" | "code"
  /** The element wterm renders into. The caller owns it, so the returned state holds no refs. */
  hostRef: React.RefObject<HTMLDivElement | null>
}) {
  const sessionID = session.id
  const [phase, setPhase] = useState<TerminalPhase>(() => (terminalCanAttach(session) ? "connecting" : "closed"))
  const [error, setError] = useState(() =>
    terminalCanAttach(session)
      ? ""
      : session.state === "closed"
        ? "This terminal has closed."
        : "This terminal is closing or failed.",
  )
  const [notice, setNotice] = useState("")
  const [role, setRole] = useState<TerminalRole>(session.role)
  const [mobileModifiers, setMobileModifiers] = useState<MobileModifiers>(noMobileModifiers)
  const [ready, setReady] = useState(false)
  // Transports opened after the first one; checks read it to prove a tab switch cost none.
  const [reconnects, setReconnects] = useState(0)
  // Output frames the host sent again that were already on screen.
  const [replays, setReplays] = useState(0)
  // The last typed close ("<code> <close number>"), for checks and bug reports.
  const [lastClose, setLastClose] = useState("")
  const [connectedEdge, setConnectedEdge] = useState("")
  // The window title the shell sets (OSC 0/2), seen as it happens; the server's copy lags.
  const [title, setTitle] = useState("")
  const [limit, setLimit] = useState<ApiError | null>(null)
  // Who is attached, pushed by the host (newer hosts only); null until the first such event.
  const [participants, setParticipants] = useState<HostParticipant[] | null>(null)
  const [filePasteProgress, setFilePasteProgress] = useState<FilePasteProgress | null>(null)
  const [filePasteNotice, setFilePasteNotice] = useState("")
  const [fileDragOver, setFileDragOver] = useState(false)
  const [filePasteBusy, setFilePasteBusyState] = useState(false)

  const terminalRef = useRef<WTerm | null>(null)
  const coreRef = useRef<GhosttyCore | null>(null)
  const fitRef = useRef<(() => void) | null>(null)
  const fittingRef = useRef(false)
  const apiRef = useRef<BrowserTerminalAPI | null>(null)
  const connectionIDRef = useRef("")
  const ticketEdgeRef = useRef("")
  const sessionRef = useRef(session)
  const themeRef = useRef(theme)
  const roleRef = useRef<TerminalRole>(session.role)
  const mobileModifiersRef = useRef<MobileModifiers>(noMobileModifiers)
  const phaseRef = useRef<TerminalPhase>(phase)
  const filePasteBusyRef = useRef(false)
  const insertingReceiptRef = useRef<{ sends: Promise<boolean>[] } | null>(null)
  const filePasteUploaderRef = useRef<FilePasteUploader | null>(null)
  const filePasteNoticeTimerRef = useRef<number | undefined>(undefined)
  const fileDragDepthRef = useRef(0)
  const ackSequenceRef = useRef("0")
  const pendingAckRef = useRef<{ connectionID: string; next: string; sending: boolean } | null>(null)
  const displaySequenceRef = useRef<string | null>(null)
  const resizeControlRef = useRef(false)
  const reconnectTimerRef = useRef<number | undefined>(undefined)
  const resizeTimerRef = useRef<number | undefined>(undefined)
  const ticketAbortRef = useRef<AbortController | null>(null)
  const retryDelayRef = useRef(1000)
  const disposedRef = useRef(false)
  const busyRef = useRef(false)
  const startedOnceRef = useRef(false)
  const startConnectionRef = useRef<((retry?: boolean) => Promise<void>) | null>(null)

  // Polled session details (title, participants) must not restart the terminal.
  useEffect(() => {
    sessionRef.current = session
  }, [session])
  useEffect(() => {
    themeRef.current = theme
    // Recolours what's already on screen too: the core re-resolves its cells.
    terminalRef.current?.write(paletteSequence(theme))
  }, [theme])

  const writeTerminal = useCallback((data: Uint8Array | string) => {
    terminalRef.current?.write(data)
  }, [])

  const inputLocked = () =>
    filePasteBusyRef.current || phaseRef.current !== "attached" || roleRef.current === "viewer"

  const syncInputLock = useCallback(() => {
    const input = terminalRef.current?.element.querySelector("textarea")
    if (input) input.readOnly = inputLocked()
  }, [])

  const getFilePasteUploader = useCallback(() => {
    filePasteUploaderRef.current ??= new FilePasteUploader(() => {
      const browser = apiRef.current
      const connectionID = connectionIDRef.current
      if (
        !browser ||
        !connectionID ||
        roleRef.current !== "owner" ||
        phaseRef.current !== "attached"
      )
        return null
      return { api: browser, connectionID }
    }, setFilePasteProgress)
    return filePasteUploaderRef.current
  }, [])

  const setPhaseValue = useCallback(
    (next: TerminalPhase) => {
      if (next !== "attached" && (mobileModifiersRef.current.ctrl || mobileModifiersRef.current.alt)) {
        mobileModifiersRef.current = noMobileModifiers
        setMobileModifiers(noMobileModifiers)
      }
      phaseRef.current = next
      setPhase(next)
      if (next !== "attached") setConnectedEdge("")
      syncInputLock()
    },
    [syncInputLock],
  )

  const showFilePasteNotice = useCallback((message: string) => {
    window.clearTimeout(filePasteNoticeTimerRef.current)
    setFilePasteNotice(message)
    filePasteNoticeTimerRef.current = window.setTimeout(() => setFilePasteNotice(""), 10000)
  }, [])

  const dismissFilePasteNotice = useCallback(() => {
    window.clearTimeout(filePasteNoticeTimerRef.current)
    setFilePasteNotice("")
  }, [])

  const setFilePasteBusy = useCallback(
    (value: boolean) => {
      filePasteBusyRef.current = value
      setFilePasteBusyState(value)
      syncInputLock()
      if (value) setNotice("")
    },
    [syncInputLock],
  )

  const uploadFiles = useCallback(
    async (files: File[]) => {
      if (!files.length) return
      if (roleRef.current !== "owner") {
        showFilePasteNotice(filePasteMessage("owner_only"))
        return
      }
      if (phaseRef.current !== "attached" || !connectionIDRef.current) {
        showFilePasteNotice(filePasteMessage("not_connected"))
        return
      }
      const uploader = getFilePasteUploader()
      if (!uploader || filePasteBusyRef.current) {
        showFilePasteNotice(filePasteMessage("busy"))
        return
      }
      setFilePasteBusy(true)
      dismissFilePasteNotice()
      try {
        const paste = await uploader.upload(files)
        if (roleRef.current !== "owner") {
          showFilePasteNotice(
            "Files uploaded, but owner access changed before their paths could be pasted. Find them in Paperboat Inbox.",
          )
          return
        }
        if (phaseRef.current !== "attached" || !connectionIDRef.current) {
          showFilePasteNotice(
            "Files uploaded, but the terminal disconnected before their paths could be pasted. Find them in Paperboat Inbox.",
          )
          return
        }
        const textarea = terminalRef.current?.element.querySelector("textarea")
        if (!textarea) {
          showFilePasteNotice("Files uploaded, but their paths could not be pasted. Find them in Paperboat Inbox.")
          return
        }
        let pasted = false
        const receiptInsertion = { sends: [] as Promise<boolean>[] }
        insertingReceiptRef.current = receiptInsertion
        try {
          pasted = pasteIntoWTerm(textarea, paste)
        } finally {
          insertingReceiptRef.current = null
        }
        if (!pasted) {
          showFilePasteNotice("Files uploaded, but their paths could not be pasted. Find them in Paperboat Inbox.")
          return
        }
        const sent = await Promise.all(receiptInsertion.sends)
        if (!receiptInsertion.sends.length || sent.some((result) => !result)) {
          showFilePasteNotice(
            "Files uploaded, but their paths could not be sent to the terminal. Find them in Paperboat Inbox.",
          )
          return
        }
        showFilePasteNotice("Files uploaded and their paths pasted.")
      } catch (reason) {
        const code = reason instanceof FilePasteError ? reason.code : "file_transfer_failed"
        const cleanup =
          reason instanceof FilePasteError && reason.cleanupFailed
            ? " A partial upload could not be canceled and will expire under the machine's retention policy."
            : ""
        showFilePasteNotice(filePasteMessage(code) + cleanup)
      } finally {
        setFilePasteBusy(false)
      }
    },
    [dismissFilePasteNotice, getFilePasteUploader, setFilePasteBusy, showFilePasteNotice],
  )

  const cancelUpload = useCallback(async () => {
    const uploader = filePasteUploaderRef.current
    if (!uploader || !filePasteBusyRef.current) return
    try {
      const cleaned = await uploader.cancel()
      showFilePasteNotice(
        cleaned
          ? "Upload canceled. Partial data was removed."
          : "Cancellation could not be confirmed. Any partial upload will expire under the machine's retention policy.",
      )
    } finally {
      setFilePasteBusy(false)
    }
  }, [setFilePasteBusy, showFilePasteNotice])

  // `waitMs` is the server's Retry-After; otherwise the backoff doubles up to 15 s.
  const scheduleReconnect = useCallback(
    (waitMs?: number) => {
      setPhaseValue("reconnecting")
      const wait = waitMs ?? retryDelayRef.current
      if (waitMs === undefined) retryDelayRef.current = Math.min(wait * 2, 15000)
      window.clearTimeout(reconnectTimerRef.current)
      reconnectTimerRef.current = window.setTimeout(() => {
        void startConnectionRef.current?.(true)
      }, wait)
    },
    [setPhaseValue],
  )

  // Renderer: one Ghostty core and wterm per session.
  useEffect(() => {
    const host = hostRef.current
    if (!host || !terminalCanAttach(sessionRef.current)) return
    let disposed = false
    let terminal: WTerm | null = null
    let core: GhosttyCore | null = null
    let observer: ResizeObserver | null = null

    const sendInput = (data: Uint8Array) => {
      const receiptInsertion = insertingReceiptRef.current
      if (filePasteBusyRef.current && !receiptInsertion) {
        setNotice("Terminal input is paused while files upload.")
        return
      }
      if (
        roleRef.current === "viewer" ||
        phaseRef.current !== "attached" ||
        !connectionIDRef.current ||
        !apiRef.current
      ) {
        if (receiptInsertion) receiptInsertion.sends.push(Promise.resolve(false))
        return
      }
      const sent = apiRef.current
        .sendInput(connectionIDRef.current, data)
        .then((result) => {
          checkResult(result)
          return true
        })
        .catch(() => {
          setNotice("Input could not be sent. Reconnecting will not close the remote process.")
          return false
        })
      if (receiptInsertion) receiptInsertion.sends.push(sent)
    }
    const fit = () => {
      if (!terminal || !host.isConnected) return
      const style = getComputedStyle(host)
      const cellWidth = Number.parseFloat(style.getPropertyValue("--term-cell-width"))
      const rowHeight = Number.parseFloat(style.getPropertyValue("--term-row-height"))
      if (!(cellWidth > 0) || !(rowHeight > 0)) return
      const columns = Math.min(500, Math.max(1, Math.floor(host.clientWidth / cellWidth)))
      const rows = Math.min(200, Math.max(1, Math.floor(host.clientHeight / rowHeight)))
      if (columns === terminal.cols && rows === terminal.rows) return
      fittingRef.current = true
      try {
        terminal.resize(columns, rows)
      } finally {
        fittingRef.current = false
      }
    }
    const onPasteCapture = (event: ClipboardEvent) => {
      const selection = classifyFilePaste(event.clipboardData)
      if (selection.kind === "text") return
      event.preventDefault()
      event.stopImmediatePropagation()
      if (selection.kind === "files") void uploadFiles(selection.files)
      else showFilePasteNotice(filePasteMessage(selection.kind))
    }
    const hasFiles = (transfer: DataTransfer | null) => !!transfer && Array.from(transfer.types ?? []).includes("Files")
    const onDragEnter = (event: DragEvent) => {
      if (!hasFiles(event.dataTransfer)) return
      event.preventDefault()
      fileDragDepthRef.current++
      setFileDragOver(true)
    }
    const onDragOver = (event: DragEvent) => {
      if (!hasFiles(event.dataTransfer)) return
      event.preventDefault()
      if (event.dataTransfer) event.dataTransfer.dropEffect = "copy"
      setFileDragOver(true)
    }
    const onDragLeave = (event: DragEvent) => {
      if (!hasFiles(event.dataTransfer)) return
      fileDragDepthRef.current = Math.max(0, fileDragDepthRef.current - 1)
      if (fileDragDepthRef.current === 0) setFileDragOver(false)
    }
    const onDrop = (event: DragEvent) => {
      const selection = classifyFilePaste(event.dataTransfer)
      fileDragDepthRef.current = 0
      setFileDragOver(false)
      if (selection.kind === "text") return
      event.preventDefault()
      event.stopImmediatePropagation()
      if (selection.kind === "files") void uploadFiles(selection.files)
      else showFilePasteNotice(filePasteMessage(selection.kind))
    }

    void (async () => {
      try {
        core = await GhosttyCore.load({
          wasmPath: "/terminal/ghostty-vt-0.5.1.wasm",
          scrollbackLimit: 65536,
          foregroundColor: themeRef.current.foreground,
          backgroundColor: themeRef.current.background,
        })
        if (disposed) {
          core.dispose()
          return
        }
        terminal = new WTerm(host, {
          core,
          autoResize: false,
          cursorBlink: true,
          onData: (value) => {
            if (filePasteBusyRef.current && !insertingReceiptRef.current) {
              setNotice("Terminal input is paused while files upload.")
              return
            }
            if (phaseRef.current !== "attached") return
            const receiptInsertion = insertingReceiptRef.current !== null
            const modifiers = receiptInsertion ? noMobileModifiers : mobileModifiersRef.current
            if (!receiptInsertion && (modifiers.ctrl || modifiers.alt)) {
              mobileModifiersRef.current = noMobileModifiers
              setMobileModifiers(noMobileModifiers)
            }
            sendInput(new TextEncoder().encode(applyMobileModifiers(value, modifiers)))
          },
          onBinary: sendInput,
          onTitle: (next) => setTitle(next.slice(0, 256)),
          onResize: (columns, rows) => {
            if (
              !fittingRef.current ||
              !resizeControlRef.current ||
              roleRef.current !== "owner" ||
              phaseRef.current !== "attached" ||
              !connectionIDRef.current ||
              !apiRef.current
            )
              return
            window.clearTimeout(resizeTimerRef.current)
            resizeTimerRef.current = window.setTimeout(() => {
              void apiRef.current
                ?.resize(connectionIDRef.current, columns, rows)
                .then(checkResult)
                .catch(() => setNotice("The terminal size could not be changed."))
            }, 120)
          },
        })
        await terminal.init()
        terminal.write(paletteSequence(themeRef.current))
        if (disposed) {
          terminal.destroy()
          core.dispose()
          return
        }
        host.style.height = "100%"
        const input = host.querySelector("textarea")
        if (input) {
          input.setAttribute("autocapitalize", "none")
          input.setAttribute("enterkeyhint", "enter")
          if (/Android/i.test(navigator.userAgent)) input.setAttribute("inputmode", "url")
          if (matchMedia("(pointer: coarse)").matches) input.blur()
        }
        terminalRef.current = terminal
        coreRef.current = core
        fitRef.current = fit
        observer = new ResizeObserver(() => {
          if (resizeControlRef.current && roleRef.current === "owner" && phaseRef.current === "attached") fit()
        })
        observer.observe(host)
        host.addEventListener("paste", onPasteCapture, true)
        host.addEventListener("dragenter", onDragEnter, true)
        host.addEventListener("dragover", onDragOver, true)
        host.addEventListener("dragleave", onDragLeave, true)
        host.addEventListener("drop", onDrop, true)
        setReady(true)
      } catch (reason) {
        terminal?.destroy()
        core?.dispose()
        if (disposed) return
        setPhaseValue("transport-error")
        setError(
          reason instanceof Error
            ? `The terminal renderer could not start: ${reason.message}`
            : "The terminal renderer could not start.",
        )
      }
    })()
    return () => {
      disposed = true
      setReady(false)
      observer?.disconnect()
      host.removeEventListener("paste", onPasteCapture, true)
      host.removeEventListener("dragenter", onDragEnter, true)
      host.removeEventListener("dragover", onDragOver, true)
      host.removeEventListener("dragleave", onDragLeave, true)
      host.removeEventListener("drop", onDrop, true)
      window.clearTimeout(resizeTimerRef.current)
      terminal?.destroy()
      core?.dispose()
      terminalRef.current = null
      coreRef.current = null
      fitRef.current = null
    }
  }, [sessionID, hostRef, uploadFiles, setPhaseValue, showFilePasteNotice])

  const acknowledge = useCallback((connectionID: string, next: string) => {
    if (
      !apiRef.current ||
      connectionIDRef.current !== connectionID ||
      compareSequence(next, ackSequenceRef.current) <= 0
    )
      return
    let entry = pendingAckRef.current
    if (!entry || entry.connectionID !== connectionID) {
      entry = { connectionID, next, sending: false }
      pendingAckRef.current = entry
    } else if (compareSequence(next, entry.next) > 0) entry.next = next
    if (entry.sending) return
    entry.sending = true
    const active = entry
    // Keep one ACK in flight: pipelined output only advances this cursor, so it cannot pile
    // up promise callbacks or crowd input out of the worker's mailbox.
    void (async () => {
      try {
        while (pendingAckRef.current === active && connectionIDRef.current === connectionID) {
          const cursor = active.next
          checkResult(await apiRef.current!.ack(connectionID, cursor))
          if (connectionIDRef.current !== connectionID) return
          if (compareSequence(cursor, ackSequenceRef.current) > 0) ackSequenceRef.current = normalizeSequence(cursor)
          if (active.next === cursor) return
        }
      } catch {
        // The next output retries; a reconnect resumes from the last accepted ACK.
      } finally {
        active.sending = false
      }
    })()
  }, [])

  const dropConnection = useCallback((connectionID: string) => {
    connectionIDRef.current = ""
    apiRef.current?.leave(connectionID)
  }, [])

  const handleEvent = useCallback(
    (connectionID: string, pin: RootPin, event: BrowserTerminalEvent) => {
      if (disposedRef.current || connectionIDRef.current !== connectionID) return
      const terminal = terminalRef.current
      if (event.type === "event" && event.capability === "terminal.v1") {
        const payload = event.payload as { event?: string; session_id?: string; participants?: HostParticipant[] }
        if (payload?.event === "participants" && payload.session_id === sessionRef.current.id && Array.isArray(payload.participants))
          setParticipants(payload.participants)
        return
      }
      if (event.type === "identity-verified") {
        const current = sessionRef.current
        if (
          event.owner_account_id !== current.owner_account ||
          event.machine_id !== current.target.machine_id ||
          event.root_key_id !== pin.rootKeyID
        ) {
          setPhaseValue("denied")
          setError("The machine's identity did not match this terminal.")
          apiRef.current?.leave(connectionID)
        }
        return
      }
      if (event.type === "state") {
        if (event.state === "connecting") {
          setPhaseValue("connecting")
          return
        }
        if (!["reconnecting", "denied", "closed", "transport-error"].includes(event.state ?? "")) return
        // Every end of a connection is one typed outcome (close-codes.ts): the words and the next step.
        const outcome = terminalCloseCodes[workerCloseCode(event) ?? "edge_unavailable"]
        connectionIDRef.current = ""
        setLastClose(`${event.code ?? event.state} ${event.close ?? ""}`.trim())
        if (outcome.phase === "reconnect") {
          setError(outcome.message)
          scheduleReconnect()
          return
        }
        window.clearTimeout(reconnectTimerRef.current)
        if (outcome.phase !== "transport-error") void filePasteUploaderRef.current?.cancel()
        setPhaseValue(outcome.phase)
        setError(
          outcome.phase === "denied" && accessMethod === "code"
            ? "Your code has stopped working. It may have expired or been replaced; ask for a new one."
            : outcome.message,
        )
        return
      }
      if (event.type === "attached") {
        const core = coreRef.current
        if (
          !terminal ||
          !core ||
          !(event.screen instanceof Uint8Array) ||
          !event.screen_sequence ||
          !event.columns ||
          !event.rows
        ) {
          setPhaseValue("transport-error")
          setError("The machine did not send a usable screen. Reconnect and try again.")
          apiRef.current?.leave(connectionID)
          return
        }
        setPhaseValue("attached")
        setConnectedEdge(ticketEdgeRef.current)
        retryDelayRef.current = 1000
        setError("")
        const nextRole: TerminalRole = event.role === "owner" || event.role === "interactive" ? event.role : "viewer"
        roleRef.current = nextRole
        setRole(nextRole)
        syncInputLock()
        if (nextRole === "viewer") {
          terminal.element.querySelector("textarea")?.blur()
          mobileModifiersRef.current = noMobileModifiers
          setMobileModifiers(noMobileModifiers)
        }
        resizeControlRef.current = nextRole === "owner" && !!event.first_attachment
        const screenSequence = normalizeSequence(event.screen_sequence)
        try {
          // The host's screen is authoritative on every attach. Reset through wterm so partial
          // escape sequences and stale scrollback cannot leak across attachments.
          writeTerminal("\x1bc")
          // The reset also restores Ghostty's default palette.
          writeTerminal(paletteSequence(themeRef.current))
          terminal.resize(event.columns, event.rows)
          writeTerminal(event.screen)
          displaySequenceRef.current = screenSequence
          ackSequenceRef.current = screenSequence
        } catch {
          setPhaseValue("transport-error")
          setError("The terminal screen could not be restored. Reconnect and try again.")
          apiRef.current?.leave(connectionID)
          return
        }
        requestAnimationFrame(() => {
          if (connectionIDRef.current === connectionID && resizeControlRef.current) fitRef.current?.()
        })
        return
      }
      if (event.type === "output" && event.data && event.start_sequence && event.next_sequence) {
        const start = normalizeSequence(event.start_sequence)
        const next = normalizeSequence(event.next_sequence)
        const current = displaySequenceRef.current
        if (current !== null && compareSequence(next, current) <= 0) {
          setReplays((count) => count + 1)
          acknowledge(connectionID, current)
          return
        }
        if (current !== null && compareSequence(start, current) > 0) {
          setError("Some output was lost. Restoring the current screen.")
          dropConnection(connectionID)
          scheduleReconnect()
          return
        }
        const data =
          current !== null && compareSequence(start, current) < 0
            ? event.data.subarray(sequenceOffset(current, start))
            : event.data
        try {
          if (event.channel === 2) {
            const geometry = /^\x1b\[8;([1-9]\d{0,2});([1-9]\d{0,2})t$/.exec(new TextDecoder().decode(data))
            if (!geometry || Number(geometry[1]) > 200 || Number(geometry[2]) > 500) throw new Error("invalid geometry")
            terminal?.resize(Number(geometry[2]), Number(geometry[1]))
          } else if (data.byteLength) {
            writeTerminal(data)
          }
          displaySequenceRef.current = next
          acknowledge(connectionID, next)
        } catch {
          setError("The output could not be displayed. Restoring the current screen.")
          dropConnection(connectionID)
          scheduleReconnect()
        }
      }
    },
    [
      accessMethod,
      acknowledge,
      dropConnection,
      scheduleReconnect,
      setPhaseValue,
      syncInputLock,
      writeTerminal,
    ],
  )

  const startConnection = useCallback(
    async (retry = false) => {
      if (busyRef.current || disposedRef.current || !terminalRef.current) return
      const current = sessionRef.current
      busyRef.current = true
      setError("")
      setPhaseValue("connecting")
      let identityID = ""
      const controller = new AbortController()
      ticketAbortRef.current = controller
      try {
        const browser = await loadBrowserTerminal()
        if (disposedRef.current) {
          browser.dispose()
          return
        }
        apiRef.current = browser
        const identity = await browser.createIdentity()
        if ("error" in identity) throw new Error(identity.message)
        identityID = identity.id
        if (disposedRef.current) {
          browser.discardIdentity(identityID)
          browser.dispose()
          identityID = ""
          return
        }
        const ticketPath = `terminal-sessions/${encodeURIComponent(current.id)}/${accessMethod === "code" ? "guest-browser-ticket" : "browser-ticket"}`
        const ticket = await (accessMethod === "code" ? guestApi<Ticket> : api<Ticket>)(ticketPath, {
          method: "POST",
          headers: { "content-type": "application/json" },
          body: JSON.stringify({ browser_public_key_sha256: identity.public_key_sha256 }),
          cache: "no-store",
          signal: controller.signal,
        })
        if (disposedRef.current) {
          browser.discardIdentity(identityID)
          identityID = ""
          return
        }
        if (ticket.owner_account_id !== current.owner_account || ticket.machine_id !== current.target.machine_id)
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
        setRole(ticket.role)
        roleRef.current = ticket.role
        const config = {
          url: streamURL.href,
          subprotocols: ticket.subprotocols,
          owner_account_id: current.owner_account,
          machine_id: current.target.machine_id,
          root_key_id: pin.rootKeyID,
          root_public_key: pin.publicKey,
          root_fingerprint: pin.fingerprint,
          terminal_session_id: current.id,
          role: ticket.role,
          from_sequence: ackSequenceRef.current,
          at_live_boundary: true,
        }
        let startedID = ""
        ticketEdgeRef.current = ticket.edge_node_id
        if (startedOnceRef.current) setReconnects((count) => count + 1)
        startedOnceRef.current = true
        const started = await browser.connect(identity.id, config, (event) => handleEvent(startedID, pin, event))
        if (!started.id) throw new Error(started.message || "The encrypted connection could not be started.")
        startedID = started.id
        if (disposedRef.current) {
          browser.leave(startedID)
          return
        }
        connectionIDRef.current = startedID
        identityID = ""
      } catch (reason) {
        if (identityID) apiRef.current?.discardIdentity(identityID)
        if (disposedRef.current) return
        const code = ticketCloseCode(reason)
        const outcome = code && terminalCloseCodes[code]
        if (outcome) {
          setError(outcome.message)
          if (outcome.phase === "reconnect") {
            scheduleReconnect(outcome.silent ? 0 : reason instanceof ApiError ? reason.retryAfterMs : undefined)
          } else {
            if (code === "limit") setLimit(reason as ApiError)
            setPhaseValue(outcome.phase)
            if (outcome.phase === "denied" && accessMethod === "code")
              setError("Your code has stopped working. It may have expired or been replaced; ask for a new one.")
          }
        } else if (retry) {
          setError("The terminal service could not be reached. Retrying with a fresh key and ticket.")
          scheduleReconnect()
        } else {
          setError(apiErrorMessage(reason))
          setPhaseValue("transport-error")
        }
      } finally {
        if (ticketAbortRef.current === controller) ticketAbortRef.current = null
        busyRef.current = false
      }
    },
    [accessMethod, handleEvent, scheduleReconnect, setPhaseValue],
  )

  useEffect(() => {
    startConnectionRef.current = (retry) => startConnection(retry)
  }, [startConnection])

  useEffect(() => {
    if (ready && phaseRef.current === "connecting" && !connectionIDRef.current) void startConnection()
  }, [ready, startConnection])

  useEffect(() => {
    disposedRef.current = false
    return () => {
      disposedRef.current = true
      window.clearTimeout(reconnectTimerRef.current)
      window.clearTimeout(filePasteNoticeTimerRef.current)
      ticketAbortRef.current?.abort()
      const browser = apiRef.current
      const connectionID = connectionIDRef.current
      const cleanup = filePasteUploaderRef.current?.dispose() ?? Promise.resolve(true)
      const finish = () => {
        if (connectionID) browser?.leave(connectionID)
        connectionIDRef.current = ""
        browser?.dispose()
        if (apiRef.current === browser) apiRef.current = null
      }
      if (filePasteBusyRef.current && browser) void cleanup.finally(finish)
      else finish()
    }
  }, [])

  const reconnect = useCallback(() => {
    if (terminalRef.current) void startConnection()
    else window.location.reload()
  }, [startConnection])

  /** Owner only: size the remote terminal to this window. */
  const fitToWindow = useCallback(() => {
    resizeControlRef.current = true
    fitRef.current?.()
  }, [])

  const toggleMobileModifier = useCallback((modifier: "ctrl" | "alt") => {
    if (inputLocked()) return
    const next = { ...mobileModifiersRef.current, [modifier]: !mobileModifiersRef.current[modifier] }
    mobileModifiersRef.current = next
    setMobileModifiers(next)
  }, [])

  const sendMobileKey = useCallback((key: MobileKey) => {
    if (!terminalRef.current || inputLocked()) return
    const modifiers = mobileModifiersRef.current
    mobileModifiersRef.current = noMobileModifiers
    setMobileModifiers(noMobileModifiers)
    const sequence = mobileKeySequence(key, coreRef.current?.cursorKeysApp() ?? false, modifiers)
    const browser = apiRef.current
    if (!browser || !connectionIDRef.current) {
      setNotice("Input could not be sent. Reconnecting will not close the remote process.")
      return
    }
    void browser
      .sendInput(connectionIDRef.current, new TextEncoder().encode(sequence))
      .then(checkResult)
      .catch(() => setNotice("Input could not be sent. Reconnecting will not close the remote process."))
  }, [])

  const focus = useCallback(() => terminalRef.current?.element.querySelector("textarea")?.focus(), [])

  return {
    phase,
    role,
    reconnects,
    replays,
    lastClose,
    error,
    notice,
    connectedEdge,
    title,
    limit,
    participants,
    reconnect,
    fitToWindow,
    focus,
    upload: {
      progress: filePasteProgress,
      notice: filePasteNotice,
      dragOver: fileDragOver,
      busy: filePasteBusy,
      start: uploadFiles,
      cancel: cancelUpload,
      dismiss: dismissFilePasteNotice,
    },
    mobile: { modifiers: mobileModifiers, toggle: toggleMobileModifier, send: sendMobileKey },
  }
}

export type TerminalConnection = ReturnType<typeof useTerminalConnection>

/** One attachment as the host reports it: identities only; names come from /sharing. */
export type HostParticipant = {
  attachment_id: string
  account_id: string
  client_id: string
  role: "owner" | "viewer" | "interactive"
  connected_at: string
}
