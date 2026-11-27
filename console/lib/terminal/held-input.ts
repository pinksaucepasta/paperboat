// Keys typed while the terminal is not attached yet: held in order and sent once, when it attaches.

export const heldInputLimit = 65536

/** Whether keys typed in this phase are held until the terminal attaches. "connecting" covers both the first
 *  connect and every reconnect attempt (the old screen and its prompt stay up meanwhile). */
export function holdsInput(phase: string): boolean {
  return phase === "connecting" || phase === "reconnecting"
}

export class HeldInput {
  private chunks: Uint8Array[] = []
  private bytes = 0
  private full = false

  /** False when the chunk does not fit. Nothing after it is taken either, so what is sent has no gap:
   *  an Enter must not run a line whose middle is missing. */
  add(chunk: Uint8Array): boolean {
    if (this.full || this.bytes + chunk.byteLength > heldInputLimit) {
      this.full = true
      return false
    }
    this.chunks.push(chunk.slice())
    this.bytes += chunk.byteLength
    return true
  }

  /** True when keys were refused since the last take: what take() returns is only the first part. */
  get cut(): boolean {
    return this.full
  }

  /** Everything held, as one write (null when nothing is), and empties the buffer. */
  take(): Uint8Array | null {
    const { chunks, bytes } = this
    this.chunks = []
    this.bytes = 0
    this.full = false
    if (!bytes) return null
    const keys = new Uint8Array(bytes)
    let at = 0
    for (const chunk of chunks) {
      keys.set(chunk, at)
      at += chunk.byteLength
    }
    return keys
  }
}
