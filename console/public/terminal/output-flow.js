const OUTPUT_LIMIT = 512 << 10
// A flood is thousands of small records a second; 8 batches of 32 events in flight is already 256 of them.
// Bytes bound the memory (OUTPUT_LIMIT); the count only bounds per-record overhead.
const RECORD_LIMIT = 8192
const MAX_SCREENS = 4
const RECORD_BYTES = 128 << 10
const BATCH_BYTES = 64 << 10
const SCREEN_BYTES = 4 << 20
const BATCH_EVENTS = 32
const MAX_INFLIGHT = OUTPUT_LIMIT / BATCH_BYTES

// Credits cover queued and posted data until the renderer reports consumption.
// Store sizes separately: postMessage may detach the transferred buffers.
export class TerminalOutputFlow {
  constructor({ send, overflow, schedule = (callback) => setTimeout(callback, 0), cancel = clearTimeout }) {
    this.send = send
    this.overflow = overflow
    this.schedule = schedule
    this.cancel = cancel
    this.pending = []
    this.inflight = new Map()
    this.handle = null
    this.closed = false
    this.bytes = 0
    this.allocatedBytes = 0
    this.records = 0
    this.screens = 0
    this.nextID = 1
  }

  push(event) {
    if (this.closed) return false
    const output = event.type === "output"
    const screen = event.type === "attached"
    const bytes = output && event.data instanceof Uint8Array ? event.data.byteLength : 0
    const screenBytes = screen && event.screen instanceof Uint8Array ? event.screen.byteLength : 0
    if ((output && (!(event.data instanceof Uint8Array) || bytes > RECORD_BYTES)) ||
        (screen && (!(event.screen instanceof Uint8Array) || screenBytes > SCREEN_BYTES || this.screens >= MAX_SCREENS))) {
      this.fail()
      return false
    }
    // A screen restore replaces everything the page has not been sent yet.
    if (screen) this.supersede()
    const previous = this.pending.at(-1)
    // Browser channel 2 carries geometry and must remain a parse barrier.
    const merge = previous && output && event.channel === 1 &&
      previous.event.type === "output" && previous.event.connection_id === event.connection_id &&
      previous.event.channel === event.channel && previous.event.stream_id === event.stream_id &&
      typeof event.start_sequence === "string" && previous.event.next_sequence === event.start_sequence &&
      previous.bytes + bytes <= BATCH_BYTES
    let capacity = bytes
    if (merge) {
      capacity = previous.capacity
      while (capacity < previous.bytes + bytes) capacity = Math.min(BATCH_BYTES, Math.max(1, capacity * 2))
    }
    const extraCapacity = merge ? capacity - previous.capacity : capacity
    // The old slab remains live while growth copies it. Reserve both buffers
    // before allocating, so even this transient peak stays within the bound.
    const allocation = merge && extraCapacity === 0 ? 0 : capacity
    if (this.allocatedBytes + allocation > OUTPUT_LIMIT || (!merge && this.records + 1 > RECORD_LIMIT)) {
      this.fail()
      return false
    }
    this.bytes += bytes
    this.allocatedBytes += extraCapacity
    if (merge) {
      if (capacity !== previous.capacity) {
        const slab = new Uint8Array(capacity)
        slab.set(previous.event.data)
        previous.slab = slab
        previous.capacity = capacity
      }
      previous.slab.set(event.data, previous.bytes)
      previous.bytes += bytes
      previous.event = { ...previous.event, data: previous.slab.subarray(0, previous.bytes), next_sequence: event.next_sequence }
    } else {
      // Own only the meaningful bytes, even when the caller passes a subarray
      // of a larger buffer. Geometric growth keeps later appends linear and
      // never retains one array per original record.
      const slab = output ? event.data.slice() : null
      const ownedEvent = output ? { ...event, data: slab } :
        screen ? { ...event, screen: event.screen.slice() } : event
      this.pending.push({ event: ownedEvent, slab, bytes, capacity, screen })
      this.records++
      if (screen) this.screens++
    }
    // A flood outruns a zero-delay timer: post every sealed entry now rather than let the queue reach the byte
    // limit while waiting for it. The last entry stays queued so the next record can still merge into it.
    while (!this.closed && this.inflight.size < MAX_INFLIGHT && this.pending.length > 1) this.flush()
    this.scheduleNext()
    return true
  }

  supersede() {
    for (const entry of this.pending) {
      this.bytes -= entry.bytes
      this.allocatedBytes -= entry.capacity
      this.records--
      if (entry.screen) this.screens--
    }
    this.pending = []
  }

  scheduleNext() {
    if (this.closed || this.inflight.size >= MAX_INFLIGHT || this.handle !== null || !this.pending.length) return
    this.handle = this.schedule(() => {
      this.handle = null
      this.flush()
    })
  }

  flush() {
    if (this.closed || this.inflight.size >= MAX_INFLIGHT || !this.pending.length) return
    const entries = []
    let bytes = 0
    let allocatedBytes = 0
    let records = 0
    let screens = 0
    while (this.pending.length && entries.length < BATCH_EVENTS) {
      const entry = this.pending[0]
      if (entries.length && (entry.screen || screens || bytes + entry.bytes > BATCH_BYTES)) break
      this.pending.shift()
      entries.push(entry.event)
      bytes += entry.bytes
      allocatedBytes += entry.capacity
      records++
      if (entry.screen) screens++
      if (screens || bytes >= BATCH_BYTES) break
    }
    const id = this.nextID++
    this.inflight.set(id, { bytes, allocatedBytes, records, screens })
    try {
      this.send({ id, events: entries })
    } catch {
      this.fail()
    }
    // Advance the ordered stream one bounded task at a time without waiting
    // for every batch's round trip. Shared byte/count reservations still cover
    // all posted batches until their exact consumption credits arrive.
    this.scheduleNext()
  }

  consumed(batchID) {
    if (this.closed) return
    const reservation = this.inflight.get(batchID)
    if (!reservation) return
    this.bytes -= reservation.bytes
    this.allocatedBytes -= reservation.allocatedBytes
    this.records -= reservation.records
    this.screens -= reservation.screens
    this.inflight.delete(batchID)
    this.scheduleNext()
  }

  fail() {
    if (this.closed) return
    this.close()
    this.overflow()
  }

  close() {
    if (this.closed) return
    this.closed = true
    if (this.handle !== null) this.cancel(this.handle)
    this.handle = null
    this.pending = []
    this.inflight.clear()
    this.bytes = this.allocatedBytes = this.records = this.screens = 0
  }
}
