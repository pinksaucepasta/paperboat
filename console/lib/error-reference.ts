/**
 * The id a person quotes to support for any error they saw. The server's `support_reference` (and `request_id`)
 * arrive in the error body and the Support-Reference / Request-Id headers; an error with none (the browser never got
 * an answer, a bug in the page) gets a client reference, minted once for that error and logged to the browser console.
 */
export type ServerTrace = { reference?: string; requestId?: string }

const serverReference = /^support_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/
const requestId = /^[A-Za-z0-9_.:-]{1,128}$/

/** The server's ids from an error answer: its body's fields first, else its headers. Anything malformed is dropped. */
export function traceFrom(
  response: Response,
  body?: { support_reference?: unknown; request_id?: unknown } | null,
): ServerTrace {
  const reference = body?.support_reference ?? response.headers.get("support-reference")
  const request = body?.request_id ?? response.headers.get("request-id")
  return {
    reference: typeof reference === "string" && serverReference.test(reference) ? reference : undefined,
    requestId: typeof request === "string" && requestId.test(request) ? request : undefined,
  }
}

const minted = new WeakMap<object, string>()

/** `client_` + 128 random bits (`crypto.randomUUID` exists only on HTTPS pages, so getRandomValues). */
export function newClientReference(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(16))
  return "client_" + Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("")
}

/** The reference to show for `error`: the server's, Next's server-render digest, or one minted (and logged) now. */
export function errorReference(error: unknown): string {
  if (typeof error === "object" && error !== null) {
    const { reference, digest } = error as { reference?: unknown; digest?: unknown }
    if (typeof reference === "string" && reference) return reference
    if (typeof digest === "string" && digest) return digest
    const known = minted.get(error)
    if (known) return known
    const fresh = newClientReference()
    minted.set(error, fresh)
    console.error(`[paperboat] ${fresh}`, error)
    return fresh
  }
  // A thrown string or undefined cannot be remembered per value; each call is a new id.
  const fresh = newClientReference()
  console.error(`[paperboat] ${fresh}`, error)
  return fresh
}

/** The sentence for any thrown value. */
export function errorText(error: unknown): string {
  return error instanceof Error && error.message ? error.message : "Something went wrong."
}
