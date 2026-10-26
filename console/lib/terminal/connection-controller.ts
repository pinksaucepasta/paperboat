import { ApiError } from "@/lib/api"
import type { RootPin } from "@/lib/terminal/browser-root-key"
import type { BrowserTerminalAPI, BrowserTerminalEvent } from "@/lib/terminal/browser-terminal-wasm"
import { FilePasteError, FilePasteUploader, filePasteMessage } from "@/lib/terminal/file-paste"
import { applyMobileModifiers, mobileKeySequence, noMobileModifiers, type MobileKey } from "@/lib/terminal/mobile-keys"
import {
  apiErrorMessage,
  checkResult,
  checkTicketChannel,
  checkTicketTarget,
  pasteIntoTextarea,
  terminalCanAttach,
  type ControllerDeps,
  type HostParticipant,
  type TerminalAccessMethod,
  type TerminalPhase,
  type TerminalRole,
  type TerminalScreen,
  type TerminalSession,
  type TerminalSnapshot,
  type TerminalTicket,
} from "@/lib/terminal/connection-types"
import { compareSequence, normalizeSequence, sequenceOffset } from "@/lib/terminal/sequence"
import { paletteSequence, type TerminalTheme } from "@/lib/terminal/themes"

/**
 * The browser terminal's connection as a state machine, apart from React: an end-to-end
 * encrypted channel to the machine (run by the WASM worker) feeding a screen the caller renders.
 * React subscribes to `getSnapshot`; everything else (timers, sequences, sockets) lives here.
 *
 * Phases and what moves between them:
 * - loading/connecting --connect()--> ticket, WASM connect, identity-verified --attached--> attached
 * - attached --output gap, failed sync, `reconnecting`, `connection_failed`--> reconnecting,
 *   which retries connect() after a doubling delay (1 s to 15 s, reset on attach)
 * - any --403/404/401, identity mismatch, `denied`--> denied (no retry)
 * - any --`closed`--> closed; --bandwidth_quota_exhausted--> quota;
 *   --browser_terminal_limit_reached--> limit; --other failures--> transport-error
 * The renderer only reports input and size; the caller retries with `connect()`.
 *
 * Rules the transitions keep:
 * - Every window is its own attachment: leaving or disposing touches only this connection.
 * - Reconnects resume from the last acknowledged sequence; output at or before the displayed
 *   sequence is acknowledged, not written again, and a gap past it reattaches.
 * - Input is accepted only while attached and caught up; anything else is refused (the input is
 *   read-only), never queued, so nothing is replayed into the shell later.
 * - When the page becomes visible or comes back online, a cursor probe must be answered and
 *   acknowledged within 10 s or the connection is replaced (a dead socket may never close).
 * - Only the owner whose attachment was the first (or who chose "fit to window") resizes the
 *   machine's terminal; everyone else shows the host's geometry and sends no resize.
 */

const inputFailed = "Input could not be sent. Reconnecting will not close the remote process."
const codeStopped = "Your code has stopped working. It may have expired or been replaced; ask for a new one."
const syncFailed = "The terminal could not synchronize its screen. Reconnecting."

export class TerminalConnectionController {
  private snapshot: TerminalSnapshot
  private readonly listeners = new Set<() => void>()
  private readonly deps: ControllerDeps
  private session: TerminalSession
  private theme: TerminalTheme
  private accessMethod: TerminalAccessMethod

  private screen: TerminalScreen | null = null
  /** Bumped whenever the screen comes or goes, so a stale renderer's awaits stop. */
  private screenGeneration = 0
  private api: BrowserTerminalAPI | null = null
  private connectionID = ""
  private ticketEdge = ""
  private ticketAbort: AbortController | null = null
  private busy = false
  private disposed = false
  private retryDelay = 1000
  private reconnectTimer: number | undefined
  private resizeTimer: number | undefined
  private resizeControl = false

  private ackSequence = "0"
  private pendingAck: { connectionID: string; next: string; sending: boolean } | null = null
  private displaySequence: string | null = null

  // Catch-up after the page becomes visible: ask for the host's cursor, then hold input until
  // ACKs reach it and the screen has painted.
  private syncing = false
  private visibilityEpoch = 0
  private catchupBoundary: string | null = null
  private catchupRequest = ""
  private catchupTimer: number | undefined
  private finishVisibilitySync: (() => void) | null = null

  private uploader: FilePasteUploader | null = null
  private insertingReceipt: { sends: Promise<boolean>[] } | null = null
  private uploadNoticeTimer: number | undefined

  constructor(options: {
    session: TerminalSession
    theme: TerminalTheme
    accessMethod: TerminalAccessMethod
    deps: ControllerDeps
  }) {
    this.session = options.session
    this.theme = options.theme
    this.accessMethod = options.accessMethod
    this.deps = options.deps
    const open = terminalCanAttach(options.session)
    this.snapshot = {
      phase: open ? "connecting" : "closed",
      role: options.session.role,
      catchingUp: false,
      error: open
        ? ""
        : options.session.state === "closed"
          ? "This terminal has closed."
          : "This terminal is closing or failed.",
      notice: "",
      connectedEdge: "",
      title: "",
      limit: null,
      participants: null,
      mobileModifiers: noMobileModifiers,
      uploadProgress: null,
      uploadNotice: "",
      uploadBusy: false,
      dragOver: false,
      visibility: options.deps.hidden() ? "hidden" : "visible",
      resizeControl: false,
      reconnects: 0,
    }
  }

  // --- Subscription ---

  subscribe = (listener: () => void) => {
    this.listeners.add(listener)
    return () => {
      this.listeners.delete(listener)
    }
  }

  getSnapshot = (): TerminalSnapshot => this.snapshot

  private set(patch: Partial<TerminalSnapshot>) {
    const keys = Object.keys(patch) as (keyof TerminalSnapshot)[]
    if (keys.every((key) => Object.is(this.snapshot[key], patch[key]))) return
    this.snapshot = { ...this.snapshot, ...patch }
    for (const listener of this.listeners) listener()
  }

  // --- Inputs from the caller ---

  /** Polled session details (title, participants) must not restart the terminal. */
  setSession(session: TerminalSession) {
    this.session = session
  }

  setTheme(theme: TerminalTheme) {
    this.theme = theme
    // Recolours what's already on screen too: the core re-resolves its cells.
    this.screen?.write(paletteSequence(theme))
  }

  getTheme(): TerminalTheme {
    return this.theme
  }

  setAccessMethod(accessMethod: TerminalAccessMethod) {
    this.accessMethod = accessMethod
  }

  get hasScreen(): boolean {
    return this.screen !== null
  }

  /** The renderer is ready: connect if nothing has yet. */
  attachScreen(screen: TerminalScreen) {
    this.screen = screen
    this.screenGeneration++
    if (this.snapshot.phase === "connecting" && !this.connectionID) void this.connect()
  }

  detachScreen() {
    this.screenGeneration++
    this.deps.clearTimeout(this.resizeTimer)
    this.deps.clearTimeout(this.catchupTimer)
    this.screen = null
  }

  rendererFailed(reason: unknown) {
    this.enter("transport-error")
    this.set({
      error:
        reason instanceof Error
          ? `The terminal renderer could not start: ${reason.message}`
          : "The terminal renderer could not start.",
    })
  }

  mount() {
    this.disposed = false
  }

  dispose() {
    this.disposed = true
    this.deps.clearTimeout(this.reconnectTimer)
    this.deps.clearTimeout(this.uploadNoticeTimer)
    this.ticketAbort?.abort()
    const browser = this.api
    const connectionID = this.connectionID
    const cleanup = this.uploader?.dispose() ?? Promise.resolve(true)
    const finish = () => {
      if (connectionID) browser?.leave(connectionID)
      this.connectionID = ""
      browser?.dispose()
      if (this.api === browser) this.api = null
    }
    if (this.snapshot.uploadBusy && browser) void cleanup.finally(finish)
    else finish()
  }

  // --- Phase transitions ---

  private enter(next: TerminalPhase) {
    const patch: { -readonly [K in keyof TerminalSnapshot]?: TerminalSnapshot[K] } = { phase: next }
    if (next !== "attached") {
      const modifiers = this.snapshot.mobileModifiers
      if (modifiers.ctrl || modifiers.alt) patch.mobileModifiers = noMobileModifiers
      patch.connectedEdge = ""
    }
    this.set(patch)
    this.syncInputLock()
  }

  private setResizeControl(value: boolean) {
    this.resizeControl = value
    this.set({ resizeControl: value && this.snapshot.role === "owner" })
  }

  private setCatchingUp(value: boolean) {
    this.set({ catchingUp: value })
    this.syncInputLock()
  }

  private inputLocked(): boolean {
    const { uploadBusy, phase, catchingUp, role } = this.snapshot
    return uploadBusy || phase !== "attached" || catchingUp || role === "viewer"
  }

  private syncInputLock() {
    const input = this.screen?.textarea()
    if (input) input.readOnly = this.inputLocked()
  }

  private scheduleReconnect() {
    this.enter("reconnecting")
    this.set({ reconnects: this.snapshot.reconnects + 1 })
    const wait = this.retryDelay
    this.retryDelay = Math.min(wait * 2, 15000)
    this.deps.clearTimeout(this.reconnectTimer)
    this.reconnectTimer = this.deps.setTimeout(() => {
      void this.connect(true)
    }, wait)
  }

  private dropConnection(connectionID: string) {
    this.connectionID = ""
    this.api?.leave(connectionID)
  }

  private dropAndReconnect(error: string, connectionID: string) {
    this.set({ error })
    this.dropConnection(connectionID)
    this.scheduleReconnect()
  }

  // --- Connect: WASM runtime, identity, ticket, encrypted channel ---

  /** Opens a new connection; `retry` reschedules on a transient failure instead of stopping. */
  connect = async (retry = false): Promise<void> => {
    if (this.busy || this.disposed || !this.screen) return
    const current = this.session
    this.busy = true
    this.set({ error: "" })
    this.enter("connecting")
    let identityID = ""
    const controller = new AbortController()
    this.ticketAbort = controller
    try {
      const browser = await this.deps.loadBrowserTerminal()
      if (this.disposed) {
        browser.dispose()
        return
      }
      this.api = browser
      const identity = await browser.createIdentity()
      if ("error" in identity) throw new Error(identity.message)
      identityID = identity.id
      if (this.disposed) {
        browser.discardIdentity(identityID)
        browser.dispose()
        identityID = ""
        return
      }
      const ticket = await this.deps.requestTicket(
        this.accessMethod,
        current.id,
        identity.public_key_sha256,
        controller.signal,
      )
      if (this.disposed) {
        browser.discardIdentity(identityID)
        identityID = ""
        return
      }
      if (await this.openChannel(browser, identity.id, ticket, current)) identityID = ""
    } catch (reason) {
      if (identityID) this.api?.discardIdentity(identityID)
      if (this.disposed) return
      this.connectFailed(reason, retry)
    } finally {
      if (this.ticketAbort === controller) this.ticketAbort = null
      this.busy = false
    }
  }

  /** Verifies the ticket and starts the channel. False when disposed before it started. */
  private async openChannel(
    browser: BrowserTerminalAPI,
    identityID: string,
    ticket: TerminalTicket,
    current: TerminalSession,
  ): Promise<boolean> {
    checkTicketTarget(ticket, current)
    const pin = await this.deps.verifyTicketKey(
      browser,
      ticket.owner_account_id,
      ticket.root_public_key,
      ticket.root_fingerprint,
      ticket.root_key_id,
    )
    const streamURL = checkTicketChannel(ticket, this.deps.now())
    this.set({ role: ticket.role })
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
      from_sequence: this.ackSequence,
      at_live_boundary: true,
    }
    let startedID = ""
    this.ticketEdge = ticket.edge_node_id
    const started = await browser.connect(identityID, config, (event) => this.handleEvent(startedID, pin, event))
    if (!started.id) throw new Error(started.message || "The encrypted connection could not be started.")
    startedID = started.id
    if (this.disposed) {
      browser.leave(startedID)
      return false
    }
    this.connectionID = startedID
    return true
  }

  private connectFailed(reason: unknown, retry: boolean) {
    this.set({ error: apiErrorMessage(reason) })
    if (reason instanceof ApiError && reason.code === "bandwidth_quota_exhausted") {
      this.enter("quota")
    } else if (reason instanceof ApiError && reason.code === "browser_terminal_limit_reached") {
      // Retrying can't help until a browser terminal is closed or the plan changes.
      this.set({ limit: reason })
      this.enter("limit")
    } else if (reason instanceof ApiError && [401, 403, 404].includes(reason.status)) {
      if (this.accessMethod === "code") this.set({ error: codeStopped })
      this.enter("denied")
    } else if (retry) {
      this.set({ error: "The terminal service could not be reached. Retrying with a fresh key and ticket." })
      this.scheduleReconnect()
    } else {
      this.enter("transport-error")
    }
  }

  // --- Events from the WASM worker ---

  private handleEvent(connectionID: string, pin: RootPin, event: BrowserTerminalEvent) {
    if (this.disposed || this.connectionID !== connectionID) return
    if (event.type === "event" && event.capability === "terminal.v1") {
      const payload = event.payload as { event?: string; session_id?: string; participants?: HostParticipant[] }
      if (
        payload?.event === "participants" &&
        payload.session_id === this.session.id &&
        Array.isArray(payload.participants)
      )
        this.set({ participants: payload.participants })
      return
    }
    if (event.type === "identity-verified") {
      const current = this.session
      if (
        event.owner_account_id !== current.owner_account ||
        event.machine_id !== current.target.machine_id ||
        event.root_key_id !== pin.rootKeyID
      ) {
        this.enter("denied")
        this.set({ error: "The machine's identity did not match this terminal." })
        this.api?.leave(connectionID)
      }
      return
    }
    if (event.type === "cursor" && event.request_id === this.catchupRequest && event.latest_sequence) {
      this.catchupBoundary = normalizeSequence(event.latest_sequence)
      this.catchupRequest = ""
      this.finishVisibilitySync?.()
      return
    }
    if (event.type === "cursor-error" && event.request_id === this.catchupRequest) {
      this.catchupRequest = ""
      this.syncing = false
      this.setCatchingUp(false)
      this.dropAndReconnect(syncFailed, connectionID)
      return
    }
    if (event.type === "state") {
      // Every state but these falls through, as the worker's other states carry no change.
      if (this.handleState(event)) return
    }
    if (event.type === "attached") {
      this.handleAttached(connectionID, event)
      return
    }
    if (event.type === "output" && event.data && event.start_sequence && event.next_sequence)
      this.handleOutput(connectionID, event.data, event.start_sequence, event.next_sequence, event.channel)
  }

  private resetSync() {
    this.connectionID = ""
    this.syncing = false
    this.setCatchingUp(false)
    this.catchupRequest = ""
    this.catchupBoundary = null
  }

  /** The worker's connection state. Returns whether the event was consumed. */
  private handleState(event: BrowserTerminalEvent): boolean {
    if (event.state === "connecting") {
      this.enter("connecting")
      return true
    }
    // The edge refuses a terminal at the bandwidth cap (and ends an open one) with this code.
    if (event.code === "bandwidth_quota_exhausted") {
      this.resetSync()
      this.deps.clearTimeout(this.reconnectTimer)
      this.enter("quota")
      return true
    }
    if (event.state === "reconnecting") {
      this.resetSync()
      this.set({ error: "" })
      this.scheduleReconnect()
      return true
    }
    if (event.state === "denied") {
      void this.uploader?.cancel()
      this.resetSync()
      this.enter("denied")
      this.set({
        error:
          this.accessMethod === "code"
            ? codeStopped
            : event.message || "Your access to this terminal was removed. Ask its owner to restore it.",
      })
      return true
    }
    if (event.state === "closed") {
      void this.uploader?.cancel()
      this.resetSync()
      this.enter("closed")
      this.set({ error: event.message || "This terminal has closed." })
      return true
    }
    if (event.state === "transport-error") {
      this.resetSync()
      if (event.code === "connection_failed") {
        this.set({ error: "The connection was interrupted. Reconnecting." })
        this.scheduleReconnect()
        return true
      }
      this.enter("transport-error")
      this.set({ error: event.message || "The encrypted connection failed. You can retry." })
      return true
    }
    return false
  }

  private handleAttached(connectionID: string, event: BrowserTerminalEvent) {
    const screen = this.screen
    if (!screen || !(event.screen instanceof Uint8Array) || !event.screen_sequence || !event.columns || !event.rows) {
      this.enter("transport-error")
      this.set({ error: "The machine did not send a usable screen. Reconnect and try again." })
      this.api?.leave(connectionID)
      return
    }
    this.enter("attached")
    this.set({ connectedEdge: this.ticketEdge })
    this.retryDelay = 1000
    this.set({ error: "" })
    const nextRole: TerminalRole = event.role === "owner" || event.role === "interactive" ? event.role : "viewer"
    this.set({ role: nextRole })
    this.syncInputLock()
    if (nextRole === "viewer") {
      screen.textarea()?.blur()
      this.set({ mobileModifiers: noMobileModifiers })
    }
    this.setResizeControl(nextRole === "owner" && !!event.first_attachment)
    const screenSequence = normalizeSequence(event.screen_sequence)
    try {
      // A fresh authenticated checkpoint supersedes any cursor request from the prior
      // connection; clear that synchronization before accepting input from the new screen.
      this.syncing = false
      this.catchupRequest = ""
      this.catchupBoundary = null
      this.finishVisibilitySync = null
      this.deps.clearTimeout(this.catchupTimer)
      this.setCatchingUp(true)
      // The host's screen is authoritative on every attach. Reset through wterm so partial
      // escape sequences and stale scrollback cannot leak across attachments.
      screen.write("\x1bc")
      // The reset also restores Ghostty's default palette.
      screen.write(paletteSequence(this.theme))
      screen.resize(event.columns, event.rows)
      screen.write(event.screen)
      this.displaySequence = screenSequence
      this.ackSequence = screenSequence
      if (!this.deps.hidden()) this.setCatchingUp(false)
    } catch {
      this.enter("transport-error")
      this.set({ error: "The terminal screen could not be restored. Reconnect and try again." })
      this.api?.leave(connectionID)
      return
    }
    this.deps.requestAnimationFrame(() => {
      if (this.connectionID === connectionID && this.resizeControl) this.screen?.fit()
    })
  }

  private handleOutput(
    connectionID: string,
    data: Uint8Array,
    startValue: string,
    nextValue: string,
    channel?: number,
  ) {
    const start = normalizeSequence(startValue)
    const next = normalizeSequence(nextValue)
    const current = this.displaySequence
    if (current !== null && compareSequence(next, current) <= 0) {
      this.acknowledge(connectionID, current)
      return
    }
    if (current !== null && compareSequence(start, current) > 0) {
      this.dropAndReconnect("Some output was lost. Restoring the current screen.", connectionID)
      return
    }
    const fresh =
      current !== null && compareSequence(start, current) < 0 ? data.subarray(sequenceOffset(current, start)) : data
    try {
      if (channel === 2) {
        const geometry = /^\x1b\[8;([1-9]\d{0,2});([1-9]\d{0,2})t$/.exec(new TextDecoder().decode(fresh))
        if (!geometry || Number(geometry[1]) > 200 || Number(geometry[2]) > 500) throw new Error("invalid geometry")
        this.screen?.resize(Number(geometry[2]), Number(geometry[1]))
      } else if (fresh.byteLength) {
        this.screen?.write(fresh)
      }
      this.displaySequence = next
      this.acknowledge(connectionID, next)
    } catch {
      this.dropAndReconnect("The output could not be displayed. Restoring the current screen.", connectionID)
    }
  }

  private acknowledge(connectionID: string, next: string) {
    if (!this.api || this.connectionID !== connectionID || compareSequence(next, this.ackSequence) <= 0) return
    let entry = this.pendingAck
    if (!entry || entry.connectionID !== connectionID) {
      entry = { connectionID, next, sending: false }
      this.pendingAck = entry
    } else if (compareSequence(next, entry.next) > 0) entry.next = next
    if (entry.sending) return
    entry.sending = true
    const active = entry
    // Keep one ACK in flight: pipelined output only advances this cursor, so it cannot pile
    // up promise callbacks or crowd input out of the worker's mailbox.
    void (async () => {
      try {
        while (this.pendingAck === active && this.connectionID === connectionID) {
          const cursor = active.next
          checkResult(await this.api!.ack(connectionID, cursor))
          if (this.connectionID !== connectionID) return
          if (compareSequence(cursor, this.ackSequence) > 0) this.ackSequence = normalizeSequence(cursor)
          this.finishVisibilitySync?.()
          if (active.next === cursor) return
        }
      } catch {
        // The next output retries; a reconnect resumes from the last accepted ACK.
      } finally {
        active.sending = false
      }
    })()
  }

  // --- Page visibility: catch up to the host's cursor before accepting input ---

  private abandonSync(error?: string) {
    this.syncing = false
    this.deps.clearTimeout(this.catchupTimer)
    this.setCatchingUp(false)
    if (error) this.set({ error })
    const connectionID = this.connectionID
    this.connectionID = ""
    if (connectionID) this.api?.leave(connectionID)
    this.scheduleReconnect()
  }

  syncVisibility = async () => {
    const epoch = ++this.visibilityEpoch
    const generation = this.screenGeneration
    this.set({ visibility: this.deps.hidden() ? "hidden" : "visible" })
    if (this.deps.hidden()) {
      this.syncing = false
      this.catchupRequest = ""
      this.catchupBoundary = null
      this.finishVisibilitySync = null
      this.deps.clearTimeout(this.catchupTimer)
      return
    }
    if (this.snapshot.phase !== "attached" || !this.connectionID || !this.api) return
    this.syncing = true
    this.setCatchingUp(true)
    this.deps.clearTimeout(this.catchupTimer)
    this.catchupTimer = this.deps.setTimeout(() => {
      if (this.visibilityEpoch !== epoch || !this.syncing) return
      this.abandonSync()
    }, 10000)
    this.finishVisibilitySync = () => {
      const boundary = this.catchupBoundary
      if (!boundary || compareSequence(this.ackSequence, boundary) < 0) return
      this.finishVisibilitySync = null
      this.catchupBoundary = null
      // Keep the recovery deadline active until the paint barrier completes; occluded
      // windows can suspend animation frames without setting `hidden`.
      this.deps.requestAnimationFrame(() =>
        this.deps.requestAnimationFrame(() => {
          if (this.visibilityEpoch !== epoch || this.deps.hidden()) return
          this.syncing = false
          this.deps.clearTimeout(this.catchupTimer)
          this.setCatchingUp(false)
        }),
      )
    }
    try {
      const result = await this.api.cursor(this.connectionID)
      if (this.visibilityEpoch !== epoch || this.screenGeneration !== generation || !this.syncing) return
      if ("request_id" in result) {
        this.catchupRequest = result.request_id
        return
      }
    } catch {
      // The recovery below handles a failed cursor request.
    }
    this.abandonSync(syncFailed)
  }

  pageHidden = () => {
    if (this.deps.hidden()) return
    this.visibilityEpoch++
    this.syncing = false
    this.finishVisibilitySync = null
    this.deps.clearTimeout(this.catchupTimer)
  }

  // --- Input, size and title from the renderer ---

  setTitle = (title: string) => this.set({ title: title.slice(0, 256) })

  /** Text typed into the renderer: applies latched mobile modifiers. */
  typed = (value: string) => {
    if (this.snapshot.uploadBusy && !this.insertingReceipt) {
      this.set({ notice: "Terminal input is paused while files upload." })
      return
    }
    if (this.snapshot.catchingUp || this.snapshot.phase !== "attached") return
    const receiptInsertion = this.insertingReceipt !== null
    const modifiers = receiptInsertion ? noMobileModifiers : this.snapshot.mobileModifiers
    if (!receiptInsertion && (modifiers.ctrl || modifiers.alt)) this.set({ mobileModifiers: noMobileModifiers })
    this.sendInput(new TextEncoder().encode(applyMobileModifiers(value, modifiers)))
  }

  sendInput = (data: Uint8Array) => {
    const receiptInsertion = this.insertingReceipt
    if (this.snapshot.uploadBusy && !receiptInsertion) {
      this.set({ notice: "Terminal input is paused while files upload." })
      return
    }
    const { role, phase, catchingUp } = this.snapshot
    if (role === "viewer" || phase !== "attached" || catchingUp || !this.connectionID || !this.api) {
      if (receiptInsertion) receiptInsertion.sends.push(Promise.resolve(false))
      return
    }
    const sent = this.api
      .sendInput(this.connectionID, data)
      .then((result) => {
        checkResult(result)
        return true
      })
      .catch(() => {
        this.set({ notice: inputFailed })
        return false
      })
    if (receiptInsertion) receiptInsertion.sends.push(sent)
  }

  /** Whether the local window sizes the remote terminal right now. */
  canFit(): boolean {
    return this.resizeControl && this.snapshot.role === "owner" && this.snapshot.phase === "attached"
  }

  /** The renderer fitted itself to its host: tell the machine, debounced. */
  screenResized = (columns: number, rows: number) => {
    if (!this.canFit() || !this.connectionID || !this.api) return
    this.deps.clearTimeout(this.resizeTimer)
    this.resizeTimer = this.deps.setTimeout(() => {
      void this.api
        ?.resize(this.connectionID, columns, rows)
        .then(checkResult)
        .catch(() => this.set({ notice: "The terminal size could not be changed." }))
    }, 120)
  }

  /** Owner only: size the remote terminal to this window. */
  fitToWindow = () => {
    this.setResizeControl(true)
    this.screen?.fit()
  }

  focus = () => this.screen?.textarea()?.focus()

  // --- Mobile keys ---

  toggleMobileModifier = (modifier: "ctrl" | "alt") => {
    if (this.inputLocked()) return
    const current = this.snapshot.mobileModifiers
    this.set({ mobileModifiers: { ...current, [modifier]: !current[modifier] } })
  }

  sendMobileKey = (key: MobileKey) => {
    if (!this.screen || this.inputLocked()) return
    const modifiers = this.snapshot.mobileModifiers
    this.set({ mobileModifiers: noMobileModifiers })
    const sequence = mobileKeySequence(key, this.screen.cursorKeysApp(), modifiers)
    const browser = this.api
    if (!browser || !this.connectionID) {
      this.set({ notice: inputFailed })
      return
    }
    void browser
      .sendInput(this.connectionID, new TextEncoder().encode(sequence))
      .then(checkResult)
      .catch(() => this.set({ notice: inputFailed }))
  }

  // --- File paste and drop: upload to the machine, then paste the paths ---

  setDragOver = (dragOver: boolean) => this.set({ dragOver })

  showUploadNotice = (message: string) => {
    this.deps.clearTimeout(this.uploadNoticeTimer)
    this.set({ uploadNotice: message })
    this.uploadNoticeTimer = this.deps.setTimeout(() => this.set({ uploadNotice: "" }), 10000)
  }

  dismissUploadNotice = () => {
    this.deps.clearTimeout(this.uploadNoticeTimer)
    this.set({ uploadNotice: "" })
  }

  private setUploadBusy(value: boolean) {
    this.set(value ? { uploadBusy: true, notice: "" } : { uploadBusy: false })
    this.syncInputLock()
  }

  private canUpload(): boolean {
    return this.snapshot.phase === "attached" && !this.snapshot.catchingUp && !!this.connectionID
  }

  private getUploader(): FilePasteUploader {
    this.uploader ??= new FilePasteUploader(
      () => {
        const browser = this.api
        const connectionID = this.connectionID
        if (!browser || !connectionID || this.snapshot.role !== "owner" || !this.canUpload()) return null
        return { api: browser, connectionID }
      },
      (progress) => this.set({ uploadProgress: progress }),
    )
    return this.uploader
  }

  uploadFiles = async (files: File[]) => {
    if (!files.length) return
    if (this.snapshot.role !== "owner") {
      this.showUploadNotice(filePasteMessage("owner_only"))
      return
    }
    if (!this.canUpload()) {
      this.showUploadNotice(filePasteMessage("not_connected"))
      return
    }
    const uploader = this.getUploader()
    if (!uploader || this.snapshot.uploadBusy) {
      this.showUploadNotice(filePasteMessage("busy"))
      return
    }
    this.setUploadBusy(true)
    this.dismissUploadNotice()
    try {
      this.showUploadNotice(await this.pastePaths(await uploader.upload(files)))
    } catch (reason) {
      const code = reason instanceof FilePasteError ? reason.code : "file_transfer_failed"
      const cleanup =
        reason instanceof FilePasteError && reason.cleanupFailed
          ? " A partial upload could not be canceled and will expire under the machine's retention policy."
          : ""
      this.showUploadNotice(filePasteMessage(code) + cleanup)
    } finally {
      this.setUploadBusy(false)
    }
  }

  /** Pastes uploaded paths through the renderer so they reach the shell as typed input. */
  private async pastePaths(paste: string): Promise<string> {
    if (this.snapshot.role !== "owner")
      return "Files uploaded, but owner access changed before their paths could be pasted. Find them in Paperboat Inbox."
    if (!this.canUpload())
      return "Files uploaded, but the terminal disconnected before their paths could be pasted. Find them in Paperboat Inbox."
    const notPasted = "Files uploaded, but their paths could not be pasted. Find them in Paperboat Inbox."
    const textarea = this.screen?.textarea()
    if (!textarea) return notPasted
    let pasted = false
    const receiptInsertion = { sends: [] as Promise<boolean>[] }
    this.insertingReceipt = receiptInsertion
    try {
      pasted = pasteIntoTextarea(textarea, paste)
    } finally {
      this.insertingReceipt = null
    }
    if (!pasted) return notPasted
    const sent = await Promise.all(receiptInsertion.sends)
    if (!receiptInsertion.sends.length || sent.some((result) => !result))
      return "Files uploaded, but their paths could not be sent to the terminal. Find them in Paperboat Inbox."
    return "Files uploaded and their paths pasted."
  }

  cancelUpload = async () => {
    const uploader = this.uploader
    if (!uploader || !this.snapshot.uploadBusy) return
    try {
      const cleaned = await uploader.cancel()
      this.showUploadNotice(
        cleaned
          ? "Upload canceled. Partial data was removed."
          : "Cancellation could not be confirmed. Any partial upload will expire under the machine's retention policy.",
      )
    } finally {
      this.setUploadBusy(false)
    }
  }
}
