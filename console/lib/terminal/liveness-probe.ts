/** How long the machine has to answer a probe before the connection counts as dead. */
export const probeDeadlineMs = 5000

/**
 * One heartbeat probe in flight at a time. A tab shown again, a window focused or a network back can find a
 * connection that died without a close (sleep, VPN change, black hole): `start` sends the probe, `answered`
 * clears it, and `onSilent` runs when nothing came back in time. The deadline starts at the send, so time spent
 * hidden never counts, and nothing runs on a timer loop.
 */
export function createLivenessProbe(
  send: () => Promise<string | undefined>,
  onSilent: () => void,
  // Arrow wrappers: a browser's setTimeout throws "Illegal invocation" when called as a method of another object.
  timers: {
    set: (run: () => void, ms: number) => ReturnType<typeof setTimeout>
    clear: (timer: ReturnType<typeof setTimeout>) => void
  } = { set: (run, ms) => setTimeout(run, ms), clear: (timer) => clearTimeout(timer) },
) {
  let pending: { request: string; timer: ReturnType<typeof setTimeout> } | undefined
  let starting = false
  // The reply can be handled before `send` has returned the request id; remember it so that probe is not timed.
  let early: string | undefined
  const clear = () => {
    if (pending) timers.clear(pending.timer)
    pending = undefined
  }
  return {
    async start() {
      if (pending || starting) return
      starting = true
      early = undefined
      const request = await send().catch(() => undefined)
      starting = false
      if (!request || pending || request === early) return
      pending = {
        request,
        timer: timers.set(() => {
          pending = undefined
          onSilent()
        }, probeDeadlineMs),
      }
    },
    /** The reply (or any traffic): `request` is the probe's id; omit it for traffic that proves the path works. */
    answered(request?: string) {
      if (pending && (request === undefined || request === pending.request)) clear()
      else if (!pending && request) early = request
    },
    clear,
  }
}
