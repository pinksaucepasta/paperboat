import { describe, expect, test } from "bun:test"
import { ApiError } from "@/lib/api"
import type { BrowserTerminalAPI, BrowserTerminalEvent } from "@/lib/terminal/browser-terminal-wasm"
import { TerminalConnectionController } from "@/lib/terminal/connection-controller"
import {
  terminalDataAttributes,
  type ControllerDeps,
  type TerminalSession,
  type TerminalTicket,
} from "@/lib/terminal/connection-types"
import { terminalThemes } from "@/lib/terminal/themes"

const session = {
  id: "session-1",
  state: "attached",
  desired_state: "open",
  role: "owner",
  owner_account: "account-1",
  target: { machine_id: "machine-1" },
} as unknown as TerminalSession

function ticket(): TerminalTicket {
  return {
    stream_url: "wss://edge.example.test/terminal",
    edge_node_id: "edge-1",
    subprotocols: ["paperboat.browser-terminal.e2ee.v1", "pb-ticket.abc"],
    expires_at: new Date(Date.now() + 60_000).toISOString(),
    role: "owner",
    machine_id: "machine-1",
    owner_account_id: "account-1",
    root_key_id: "root-1",
    root_public_key: "key",
    root_fingerprint: "fingerprint",
  }
}

/** A fake WASM worker, ticket service and clock the test drives by hand. */
function harness(options: { tickets?: (() => Promise<TerminalTicket>)[]; accessMethod?: "account" | "code" } = {}) {
  const timers = new Map<number, { callback: () => void; ms: number }>()
  let nextTimer = 1
  const frames: (() => void)[] = []
  const callbacks: ((event: BrowserTerminalEvent) => void)[] = []
  const calls = {
    left: [] as string[],
    disposed: 0,
    acks: [] as string[],
    tickets: 0,
    inputs: [] as string[],
    resizes: [] as [number, number][],
    fromSequences: [] as unknown[],
  }
  const page = { hidden: false }
  const browser = {
    createIdentity: async () => ({ id: "identity", public_key_sha256: "sha" }),
    discardIdentity: () => {},
    connect: async (
      _identity: string,
      config: Record<string, unknown>,
      callback: (event: BrowserTerminalEvent) => void,
    ) => {
      callbacks.push(callback)
      calls.fromSequences.push(config.from_sequence)
      return { id: `connection-${callbacks.length}` }
    },
    ack: async (_id: string, next: string) => {
      calls.acks.push(next)
    },
    sendInput: async (_id: string, data: Uint8Array) => {
      calls.inputs.push(new TextDecoder().decode(data))
    },
    resize: async (_id: string, columns: number, rows: number) => {
      calls.resizes.push([columns, rows])
    },
    cursor: async () => ({ request_id: "request-1" }),
    leave: (id: string) => calls.left.push(id),
    dispose: () => calls.disposed++,
  } as unknown as BrowserTerminalAPI
  const deps: ControllerDeps = {
    loadBrowserTerminal: async () => browser,
    requestTicket: () => {
      const next = options.tickets?.[calls.tickets]
      calls.tickets++
      return next ? next() : Promise.resolve(ticket())
    },
    verifyTicketKey: async () => ({
      version: 1,
      ownerAccountID: "account-1",
      rootKeyID: "root-1",
      publicKey: "key",
      fingerprint: "fingerprint",
    }),
    setTimeout: (callback, ms) => {
      timers.set(nextTimer, { callback, ms })
      return nextTimer++
    },
    clearTimeout: (id) => {
      if (id !== undefined) timers.delete(id)
    },
    requestAnimationFrame: (callback) => void frames.push(callback),
    hidden: () => page.hidden,
    now: () => Date.now(),
  }
  const controller = new TerminalConnectionController({
    session,
    theme: terminalThemes[0],
    accessMethod: options.accessMethod ?? "account",
    deps,
  })
  const written: (string | Uint8Array)[] = []
  const screen = {
    write: (data: string | Uint8Array) => void written.push(data),
    resize: () => {},
    textarea: () => null,
    cursorKeysApp: () => false,
    fit: () => {},
  }
  const settle = () => new Promise((resolve) => setTimeout(resolve, 0))
  const emit = (event: BrowserTerminalEvent) => callbacks.at(-1)!(event)
  const attach = (role = "owner", first = false) =>
    emit({
      type: "attached",
      role,
      first_attachment: first,
      screen: new Uint8Array([65]),
      screen_sequence: "10",
      columns: 80,
      rows: 24,
    })
  const runTimer = (ms: number) => {
    const entry = [...timers.entries()].find(([, timer]) => timer.ms === ms)
    if (!entry) throw new Error(`no ${ms} ms timer`)
    timers.delete(entry[0])
    entry[1].callback()
  }
  const start = async () => {
    controller.mount()
    controller.attachScreen(screen)
    await settle()
  }
  const runFrames = () => {
    while (frames.length) frames.shift()!()
  }
  return { controller, timers, calls, written, page, start, settle, emit, attach, runTimer, runFrames, callbacks }
}

describe("TerminalConnectionController", () => {
  test("connect issues a ticket, opens the channel and attaches", async () => {
    const h = harness()
    expect(h.controller.getSnapshot().phase).toBe("connecting")
    await h.start()
    expect(h.calls.tickets).toBe(1)
    h.emit({ type: "identity-verified", owner_account_id: "account-1", machine_id: "machine-1", root_key_id: "root-1" })
    expect(h.controller.getSnapshot().phase).toBe("connecting")
    h.attach()
    const state = h.controller.getSnapshot()
    expect(state.phase).toBe("attached")
    expect(state.connectedEdge).toBe("edge-1")
    expect(state.role).toBe("owner")
    expect(state.catchingUp).toBe(false)
    expect(h.written.at(-1)).toEqual(new Uint8Array([65]))
  })

  test("a mismatched machine identity is denied", async () => {
    const h = harness()
    await h.start()
    h.emit({ type: "identity-verified", owner_account_id: "account-1", machine_id: "other", root_key_id: "root-1" })
    expect(h.controller.getSnapshot().phase).toBe("denied")
    expect(h.calls.left).toEqual(["connection-1"])
  })

  test("a transport error reconnects with a doubling backoff, reset on attach", async () => {
    const h = harness()
    await h.start()
    h.attach()
    h.emit({ type: "state", state: "transport-error", code: "connection_failed" })
    expect(h.controller.getSnapshot().phase).toBe("reconnecting")
    expect(h.controller.getSnapshot().connectedEdge).toBe("")
    h.runTimer(1000)
    await h.settle()
    expect(h.calls.tickets).toBe(2)
    h.emit({ type: "state", state: "transport-error", code: "connection_failed" })
    h.runTimer(2000)
    await h.settle()
    h.attach()
    expect(h.controller.getSnapshot().phase).toBe("attached")
    h.emit({ type: "state", state: "reconnecting" })
    expect([...h.timers.values()].map((timer) => timer.ms)).toEqual([1000])
  })

  test("an unreachable ticket service retries only on a retry", async () => {
    const h = harness({
      tickets: [() => Promise.reject(new ApiError(503, "unavailable", "down"))],
    })
    await h.start()
    expect(h.controller.getSnapshot().phase).toBe("transport-error")
    expect(h.timers.size).toBe(0)
  })

  test("a 403 ticket is denied with no retry", async () => {
    const h = harness({ tickets: [() => Promise.reject(new ApiError(403, "forbidden", "no"))] })
    await h.start()
    const state = h.controller.getSnapshot()
    expect(state.phase).toBe("denied")
    expect(state.error).toBe("This terminal is unavailable or your access has changed. Ask the owner.")
    expect(h.timers.size).toBe(0)
  })

  test("a guest whose code stopped working is told so", async () => {
    const h = harness({
      accessMethod: "code",
      tickets: [() => Promise.reject(new ApiError(404, "not_found", "no"))],
    })
    await h.start()
    expect(h.controller.getSnapshot().phase).toBe("denied")
    expect(h.controller.getSnapshot().error).toContain("Your code has stopped working")
  })

  test("the plan's browser-terminal limit stops at limit", async () => {
    const refusal = new ApiError(403, "browser_terminal_limit_reached", "limit")
    const h = harness({ tickets: [() => Promise.reject(refusal)] })
    await h.start()
    expect(h.controller.getSnapshot().phase).toBe("limit")
    expect(h.controller.getSnapshot().limit).toBe(refusal)
    expect(h.timers.size).toBe(0)
  })

  test("an exhausted bandwidth quota stops at quota and cancels a pending reconnect", async () => {
    const h = harness()
    await h.start()
    h.attach()
    h.emit({ type: "state", state: "transport-error", code: "connection_failed" })
    expect(h.timers.size).toBe(1)
    h.emit({ type: "state", state: "transport-error", code: "bandwidth_quota_exhausted" })
    expect(h.controller.getSnapshot().phase).toBe("reconnecting")
    // The reconnect dropped the connection, so later events for it are ignored: quota arrives
    // on the next connection.
    h.runTimer(1000)
    await h.settle()
    h.emit({ type: "state", state: "closed", code: "bandwidth_quota_exhausted" })
    expect(h.controller.getSnapshot().phase).toBe("quota")
    expect(h.timers.size).toBe(0)
  })

  test("a closed state event closes the terminal", async () => {
    const h = harness()
    await h.start()
    h.attach()
    h.emit({ type: "state", state: "closed", message: "" })
    expect(h.controller.getSnapshot().phase).toBe("closed")
    expect(h.controller.getSnapshot().error).toBe("This terminal has closed.")
    h.emit({ type: "state", state: "reconnecting" })
    expect(h.controller.getSnapshot().phase).toBe("closed")
  })

  test("output in order is written and acknowledged; a gap reconnects", async () => {
    const h = harness()
    await h.start()
    h.attach()
    h.emit({ type: "output", data: new Uint8Array([66]), start_sequence: "10", next_sequence: "11" })
    await h.settle()
    expect(h.calls.acks).toEqual(["11"])
    h.emit({ type: "output", data: new Uint8Array([67, 68]), start_sequence: "10", next_sequence: "12" })
    expect(h.written.at(-1)).toEqual(new Uint8Array([68]))
    h.emit({ type: "output", data: new Uint8Array([69]), start_sequence: "20", next_sequence: "21" })
    expect(h.controller.getSnapshot().phase).toBe("reconnecting")
    expect(h.controller.getSnapshot().error).toBe("Some output was lost. Restoring the current screen.")
    expect(h.calls.left).toEqual(["connection-1"])
  })

  test("the snapshot changes only when state does", async () => {
    const h = harness()
    let notified = 0
    h.controller.subscribe(() => notified++)
    const before = h.controller.getSnapshot()
    h.controller.setDragOver(false)
    expect(h.controller.getSnapshot()).toBe(before)
    expect(notified).toBe(0)
    h.controller.setDragOver(true)
    expect(h.controller.getSnapshot()).not.toBe(before)
    expect(notified).toBe(1)
  })

  test("dispose cancels timers and the runtime", async () => {
    const h = harness()
    await h.start()
    h.attach()
    h.controller.showUploadNotice("note")
    h.emit({ type: "state", state: "reconnecting" })
    expect(h.timers.size).toBe(2)
    h.runTimer(1000)
    await h.settle()
    h.emit({ type: "state", state: "reconnecting" })
    h.controller.dispose()
    expect(h.timers.size).toBe(0)
    expect(h.calls.disposed).toBe(1)
    await h.controller.connect()
    expect(h.calls.tickets).toBe(2)
  })

  test("hidden to visible: catch-up holds input, then output past the boundary resumes without duplicates", async () => {
    const h = harness()
    await h.start()
    h.attach()
    h.page.hidden = true
    await h.controller.syncVisibility()
    h.page.hidden = false
    await h.controller.syncVisibility()
    expect(h.controller.getSnapshot().catchingUp).toBe(true)
    // Typing while catching up is refused (the input is read-only), never queued.
    h.controller.typed("x")
    expect(h.calls.inputs).toEqual([])
    h.emit({ type: "cursor", request_id: "request-1", latest_sequence: "12" })
    h.emit({ type: "output", data: new Uint8Array([66, 67]), start_sequence: "10", next_sequence: "12" })
    // A replay of the same range is acknowledged, not written again.
    h.emit({ type: "output", data: new Uint8Array([66, 67]), start_sequence: "10", next_sequence: "12" })
    await h.settle()
    h.runFrames()
    h.runFrames()
    expect(h.controller.getSnapshot().catchingUp).toBe(false)
    expect(h.written.filter((data) => data instanceof Uint8Array && data[0] === 66)).toHaveLength(1)
    expect(h.timers.size).toBe(0)
    h.controller.typed("y")
    await h.settle()
    expect(h.calls.inputs).toEqual(["y"])
  })

  test("a visible page whose cursor never answers reconnects after 10 s", async () => {
    const h = harness()
    await h.start()
    h.attach()
    await h.controller.syncVisibility()
    h.runTimer(10000)
    expect(h.controller.getSnapshot().phase).toBe("reconnecting")
    expect(h.calls.left).toEqual(["connection-1"])
  })

  test("two windows on one session are separate attachments", async () => {
    const a = harness()
    const b = harness()
    await a.start()
    await b.start()
    a.attach()
    b.attach()
    const participants = [
      { attachment_id: "1", account_id: "account-1", client_id: "c1", role: "owner", connected_at: "" },
      { attachment_id: "2", account_id: "account-1", client_id: "c2", role: "owner", connected_at: "" },
    ]
    for (const window of [a, b])
      window.emit({
        type: "event",
        capability: "terminal.v1",
        payload: { event: "participants", session_id: "session-1", participants },
      })
    a.controller.dispose()
    expect(a.calls.left).toEqual(["connection-1"])
    expect(b.calls.left).toEqual([])
    expect(b.controller.getSnapshot().phase).toBe("attached")
    expect(b.controller.getSnapshot().participants).toEqual(participants as never)
  })

  test("a dead socket reconnects from the last acknowledged sequence and replays only newer output", async () => {
    const h = harness()
    await h.start()
    h.attach()
    h.emit({ type: "output", data: new Uint8Array([66]), start_sequence: "10", next_sequence: "11" })
    await h.settle()
    h.emit({ type: "state", state: "transport-error", code: "connection_failed" })
    // Keystrokes while reconnecting are refused, not queued.
    h.controller.typed("lost")
    h.controller.sendMobileKey("ESC")
    h.runTimer(1000)
    await h.settle()
    expect(h.calls.fromSequences).toEqual(["0", "11"])
    h.emit({
      type: "attached",
      role: "owner",
      screen: new Uint8Array([90]),
      screen_sequence: "11",
      columns: 80,
      rows: 24,
    })
    h.emit({ type: "output", data: new Uint8Array([66, 67]), start_sequence: "10", next_sequence: "12" })
    expect(h.written.at(-1)).toEqual(new Uint8Array([67]))
    expect(h.calls.inputs).toEqual([])
  })

  test("only the owner holding resize control resizes the machine's terminal", async () => {
    const later = harness()
    await later.start()
    later.attach("owner", false)
    later.controller.screenResized(100, 30)
    expect(later.timers.size).toBe(0)

    const viewer = harness()
    await viewer.start()
    viewer.attach("viewer", true)
    viewer.controller.screenResized(100, 30)
    expect(viewer.timers.size).toBe(0)

    const first = harness()
    await first.start()
    first.attach("owner", true)
    first.controller.screenResized(100, 30)
    first.runTimer(120)
    await first.settle()
    expect(first.calls.resizes).toEqual([[100, 30]])
  })

  test("the snapshot carries the DOM contract's attributes", async () => {
    const h = harness()
    await h.start()
    h.attach("owner", true)
    h.emit({ type: "state", state: "reconnecting" })
    h.page.hidden = true
    await h.controller.syncVisibility()
    expect(terminalDataAttributes(h.controller.getSnapshot())).toEqual({
      "data-terminal-phase": "reconnecting",
      "data-terminal-role": "owner",
      "data-terminal-catching-up": "false",
      "data-terminal-visibility": "hidden",
      "data-terminal-resize-control": "true",
      "data-terminal-reconnects": "1",
    })
  })
})
