import { describe, expect, test } from "bun:test"
import { TerminalOutputFlow } from "../../public/terminal/output-flow.js"

type Event = { type: string; connection_id: string; channel?: number; stream_id?: number; start_sequence?: string; next_sequence?: string; data?: Uint8Array; screen?: Uint8Array }

// The page returns a batch's credit after it has handled it; here that is the next tick.
function flowWithConsumer() {
  const sent: { id: number; events: Event[] }[] = []
  let overflowed = 0
  const queue: (() => void)[] = []
  const flow = new TerminalOutputFlow({
    send: (batch: { id: number; events: Event[] }) => {
      sent.push(batch)
      queue.push(() => flow.consumed(batch.id))
    },
    overflow: () => overflowed++,
    schedule: (callback: () => void) => {
      queue.push(callback)
      return 0 as unknown as ReturnType<typeof setTimeout>
    },
    cancel: () => undefined,
  })
  const drain = () => {
    while (queue.length) queue.shift()!()
  }
  return { flow, sent, drain, overflowed: () => overflowed }
}

const output = (n: number): Event => ({
  type: "output",
  connection_id: "c",
  channel: 1,
  stream_id: 1,
  start_sequence: String(n * 10),
  next_sequence: String(n * 10 + 10),
  data: new Uint8Array(10),
})

describe("worker output flow", () => {
  test("a flood burst of small records is delivered whole, in order, without an overflow", () => {
    const { flow, sent, drain, overflowed } = flowWithConsumer()
    // 2 000 records arrive in one go (the WASM emitted 335 in a single second), before the page consumes any.
    for (let n = 0; n < 2000; n++) expect(flow.push({ ...output(n), channel: n % 2 === 0 ? 1 : 3 })).toBe(true)
    drain()
    expect(overflowed()).toBe(0)
    const got = sent.flatMap((batch) => batch.events).length
    expect(got).toBe(2000)
  })

  test("a burst is posted as it arrives, without waiting for a timer tick", () => {
    // The worker's zero-delay timer never runs during a flood burst; the queue must not grow to the byte limit.
    const sent: { events: Event[] }[] = []
    let overflowed = 0
    const flow = new TerminalOutputFlow({
      send: (batch: { id: number; events: Event[] }) => {
        sent.push(batch)
        flow.consumed(batch.id)
      },
      overflow: () => overflowed++,
      schedule: () => 0 as unknown as ReturnType<typeof setTimeout>,
      cancel: () => undefined,
    })
    for (let n = 0; n < 5000; n++) {
      // 40 KiB records on alternating channels cannot merge: ~200 MB in all, far above the 512 KiB limit.
      flow.push({ ...output(n), channel: n % 2 === 0 ? 1 : 3, data: new Uint8Array(40 << 10) })
    }
    expect(overflowed).toBe(0)
    expect(sent.flatMap((batch) => batch.events).length).toBeGreaterThanOrEqual(4999)
  })

  test("a screen restore replaces what the page has not been sent, and a second one does not overflow", () => {
    const { flow, sent, drain, overflowed } = flowWithConsumer()
    for (let n = 0; n < 5; n++) flow.push(output(n))
    expect(flow.push({ type: "attached", connection_id: "c", screen: new Uint8Array(100) })).toBe(true)
    expect(flow.push({ type: "attached", connection_id: "c", screen: new Uint8Array(100) })).toBe(true)
    drain()
    expect(overflowed()).toBe(0)
    const types = sent.flatMap((batch) => batch.events).map((event) => event.type)
    expect(types.filter((type) => type === "attached").length).toBeGreaterThanOrEqual(1)
    expect(types.at(-1)).toBe("attached")
  })

  test("a consumer that never answers still overflows once, so memory stays bounded", () => {
    let overflowed = 0
    const flow = new TerminalOutputFlow({ send: () => undefined, overflow: () => overflowed++, schedule: () => 0 as unknown as ReturnType<typeof setTimeout>, cancel: () => undefined })
    let accepted = 0
    for (let n = 0; n < 100000 && flow.push(output(n)); n++) accepted++
    expect(overflowed).toBe(1)
    expect(accepted).toBeLessThan(100000)
  })
})
