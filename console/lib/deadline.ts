/**
 * A signal that aborts when the caller cancels or when `ms` pass, whichever comes first. A caller's
 * signal never removes the deadline: both apply.
 */
export function withDeadline(signal: AbortSignal | null | undefined, ms: number): AbortSignal {
  const deadline = AbortSignal.timeout(ms)
  return signal ? AbortSignal.any([signal, deadline]) : deadline
}
