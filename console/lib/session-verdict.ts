/**
 * What `/me` says after a request answered 401: only a successful `/me` proves the session (the
 * 401 was for an unknown path, NOTES.md); a 401 means the session ended; anything else (429, 5xx,
 * a gateway error) is an outage to show and retry, never "not available" or a sign-out.
 */
export function sessionVerdict(meStatus: number): "valid" | "ended" | "unknown" {
  if (meStatus >= 200 && meStatus < 300) return "valid"
  if (meStatus === 401) return "ended"
  return "unknown"
}
