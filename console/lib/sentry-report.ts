import { captureException } from "@sentry/core"
import { ApiError } from "@/lib/api"

const reported = new WeakSet<object>()

/**
 * Reports an error a person was shown, once per error: the unexpected ones (a bug in the page, a 5xx, no answer at all).
 * A 4xx is the control plane's answer, not a failure. maskEvent tags the event with the reference shown to the person.
 */
export function reportShownError(error: unknown) {
  if (typeof error !== "object" || error === null || reported.has(error)) return
  if (error instanceof ApiError && error.status >= 400 && error.status < 500) return
  reported.add(error)
  captureException(error)
}
