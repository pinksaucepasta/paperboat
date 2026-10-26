export type BrowserTerminalEvent = {
  type: string
  connection_id?: string
  state?: string
  code?: string
  /** Close-state events: the raw WebSocket close number, for diagnostics only; never branched on. */
  close?: number
  /** connection_lost only: why the WASM ended the socket itself; absent when it was closed from outside. */
  transport_cause?: string
  message?: string
  owner_account_id?: string
  machine_id?: string
  root_key_id?: string
  role?: string
  columns?: number
  rows?: number
  first_attachment?: boolean
  from_sequence?: string
  start_sequence?: string
  next_sequence?: string
  data?: Uint8Array
  channel?: number
  screen?: Uint8Array
  screen_sequence?: string
  capability?: string
  payload?: unknown
}

export type BrowserTerminalAPI = {
  createIdentity(): Promise<{ id: string; public_key_sha256: string } | { error: string; message: string }>
  inspectRoot(
    publicKey: string,
  ): Promise<{ root_key_id: string; fingerprint: string } | { error: string; message: string }>
  discardIdentity(id: string): void
  connect(
    identityID: string,
    config: Record<string, unknown>,
    callback: (event: BrowserTerminalEvent) => void,
  ): Promise<{ id?: string; error?: string; message?: string }>
  sendInput(id: string, data: Uint8Array): Promise<void | { error: string; message: string }>
  resize(id: string, columns: number, rows: number): Promise<void | { error: string; message: string }>
  ack(id: string, nextSequence: string): Promise<void | { error: string; message: string }>
  hashFile(file: File): Promise<{ sha256: string } | { error: string; message: string }>
  fileRequest(
    id: string,
    request: Record<string, unknown>,
  ): Promise<{ result?: unknown; error?: string; message?: string }>
  /** Inspector channels only (`mode: "inspector"`): one daemon inspector API call. */
  inspectorRequest(
    id: string,
    payload: Record<string, unknown>,
  ): Promise<{ result?: unknown; error?: string; message?: string }>
  /** This window gained focus: the machine sizes the shell to the typing window that was last active. */
  activity(id: string): Promise<void | { error: string; message: string }>
  leave(id: string): void
  dispose(): void
}

// Ported unchanged from paperboat-dashboard; the worker and WASM live in public/terminal.
// Keep private keys, TLS, broadcast verification and socket congestion off the UI
// thread. Only authenticated terminal bytes cross this boundary.
let runtimePromise: Promise<BrowserTerminalAPI> | undefined

// When the page last woke from a freeze (Page Lifecycle `resume`).
let resumedAt = 0
if (typeof document !== "undefined") document.addEventListener("resume", () => (resumedAt = performance.now()))

function nextTask(run: () => void) {
  const { port1, port2 } = new MessageChannel()
  port1.onmessage = () => {
    port1.close()
    run()
  }
  port2.postMessage(null)
}

async function startRuntime(): Promise<BrowserTerminalAPI> {
  const worker = new Worker(`/terminal/browser-terminal-worker.js?v=20261026-activity1`, {
    type: "module",
    name: "paperboat-terminal",
  })
  type Pending = { resolve: (value: unknown) => void; reject: (error: Error) => void; timer: number; bytes: number }
  const pending = new Map<number, Pending>()
  const callbacks = new Map<number, (event: BrowserTerminalEvent) => void>()
  const connections = new Map<string, number>()
  let sequence = 0
  let pendingBytes = 0
  let stopped = false
  const fail = () => {
    if (stopped) return
    stopped = true
    worker.terminate()
    runtimePromise = undefined
    for (const request of pending.values()) {
      window.clearTimeout(request.timer)
      request.reject(new Error("The terminal worker stopped. Reconnect to resume."))
    }
    pending.clear()
    pendingBytes = 0
    const active = [...connections]
    connections.clear()
    for (const [id, callbackID] of active) {
      try {
        callbacks.get(callbackID)?.({
          type: "state",
          connection_id: id,
          state: "reconnecting",
          code: "connection_failed",
        })
      } catch {
        /* Cleanup must finish even if a renderer callback failed. */
      }
    }
    callbacks.clear()
  }
  worker.onerror = fail
  worker.onmessageerror = fail
  const call = <T>(method: string, args: unknown[], bytes = 0, callbackID?: number, timeoutMs = 20000): Promise<T> => {
    if (stopped) return Promise.reject(new Error("The terminal worker stopped. Reconnect to resume."))
    // Include the transferred-but-not-yet-admitted input in the bound, rather
    // than letting the browser's worker mailbox become an unbounded queue.
    if (pending.size >= 32 || pendingBytes + bytes > (1 << 20) + 4096)
      return Promise.reject(new Error("Terminal input is congested. Wait before sending more input."))
    const id = ++sequence
    return new Promise<T>((resolve, reject) => {
      let started = performance.now()
      // A frozen page wakes with every timer due while the worker's answers still wait in the
      // queue; only a call that stayed unanswered while the page ran has failed.
      const expire = () => {
        if (resumedAt > started) {
          started = performance.now()
          timer = window.setTimeout(expire, timeoutMs)
          pending.get(id)!.timer = timer
          return
        }
        fail()
      }
      let timer = window.setTimeout(expire, timeoutMs)
      pending.set(id, { resolve: (value) => resolve(value as T), reject, timer, bytes })
      pendingBytes += bytes
      try {
        const transfer = args
          .filter((arg): arg is Uint8Array => arg instanceof Uint8Array)
          .map((arg) => arg.buffer as ArrayBuffer)
        worker.postMessage({ type: "call", id, method, args, callbackID }, transfer)
      } catch {
        fail()
      }
    })
  }
  worker.onmessage = ({ data }) => {
    if (stopped) return
    if (data.type === "fatal") {
      fail()
      return
    }
    if (data.type === "result") {
      const request = pending.get(data.id)
      if (!request) return
      pending.delete(data.id)
      pendingBytes -= request.bytes
      window.clearTimeout(request.timer)
      if (data.error) request.reject(new Error("The terminal worker could not complete the request."))
      else request.resolve(data.value)
      return
    }
    if (data.type === "events") {
      // The worker permits a bounded window in flight. Return each credit only
      // after Ghostty has parsed it, including while this page is hidden.
      // A task boundary lets connect RPC promises settle before events. A message
      // task, not a timer: Chrome delays timers in a hidden tab to once a second or
      // minute, which would starve the credit and cut the connection for backpressure.
      nextTask(() => {
        if (stopped) return
        try {
          for (const event of data.batch.events as BrowserTerminalEvent[]) {
            const callbackID = connections.get(event.connection_id ?? "")
            if (callbackID !== undefined) callbacks.get(callbackID)?.(event)
            if (
              event.type === "state" &&
              ["reconnecting", "denied", "closed", "transport-error"].includes(event.state ?? "")
            )
              api.leave(event.connection_id ?? "")
          }
        } catch {
          fail()
          return
        }
        worker.postMessage({ type: "consumed", callbackID: data.callbackID, id: data.batch.id })
      })
    }
  }
  const api: BrowserTerminalAPI = {
    createIdentity: () => call("createIdentity", []),
    inspectRoot: (key) => call("inspectRoot", [key]),
    discardIdentity: (id) => {
      void call("discardIdentity", [id]).catch(() => undefined)
    },
    connect: async (identityID, config, callback) => {
      const callbackID = ++sequence
      callbacks.set(callbackID, callback)
      try {
        const result = await call<{ id?: string; error?: string; message?: string }>(
          "connect",
          [identityID, config],
          0,
          callbackID,
        )
        if (result.id) connections.set(result.id, callbackID)
        else callbacks.delete(callbackID)
        return result
      } catch (error) {
        callbacks.delete(callbackID)
        throw error
      }
    },
    sendInput: (id, data) => {
      if (data.byteLength > 1 << 20)
        return Promise.resolve({ error: "input_limit", message: "Terminal input is too large." })
      // Transfer only our own buffer: callers may still own a subarray.
      const copy = data.slice()
      return call("sendInput", [id, copy], copy.byteLength)
    },
    resize: (id, columns, rows) => call("resize", [id, columns, rows]),
    activity: (id) => call("activity", [id]),
    ack: (id, next) => call("ack", [id, next]),
    hashFile: (file) => call("hashFile", [file]),
    fileRequest: (id, request) => {
      const bytes = new TextEncoder().encode(JSON.stringify(request)).byteLength
      if (bytes > 48 << 10)
        return Promise.resolve({ error: "file_limit", message: "The file request exceeds the upload limit." })
      return call("fileRequest", [id, request], bytes)
    },
    inspectorRequest: (id, payload) => {
      const bytes = new TextEncoder().encode(JSON.stringify(payload)).byteLength
      if (bytes > 48 << 10)
        return Promise.resolve({ error: "inspector_limit", message: "The inspector request is too large." })
      return call("inspectorRequest", [id, payload], bytes)
    },
    leave: (id) => {
      const callbackID = connections.get(id)
      connections.delete(id)
      if (callbackID !== undefined) callbacks.delete(callbackID)
      // Cleanup must remain admissible even when the command queue is full.
      if (!stopped) worker.postMessage({ type: "leave", id, callbackID })
    },
    dispose: () => {
      // Clear callbacks before terminating so explicit unmount never reconnects.
      connections.clear()
      callbacks.clear()
      fail()
    },
  }
  await call("ready", [])
  return api
}

export function loadBrowserTerminal(): Promise<BrowserTerminalAPI> {
  runtimePromise ??= startRuntime().catch((error: unknown) => {
    runtimePromise = undefined
    throw error
  })
  return runtimePromise
}
