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
  timers: { set: typeof setTimeout; clear: typeof clearTimeout } = { set: setTimeout, clear: clearTimeout },
) {
  let pending: { request: string; timer: ReturnType<typeof setTimeout> } | undefined
  let starting = false
  const clear = () => {
    if (pending) timers.clear(pending.timer)
    pending = undefined
  }
  return {
    async start() {
      if (pending || starting) return
      starting = true
      const request = await send().catch(() => undefined)
      starting = false
      if (!request || pending) return
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
    },
    clear,
  }
}
