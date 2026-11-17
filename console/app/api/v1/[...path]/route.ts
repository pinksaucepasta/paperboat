import {
  consoleOrigin,
  controlPlaneFetch,
  csrfCredentials,
  isSameOrigin,
  relayCookies,
} from "@/lib/server/control-plane"

/**
 * Same-origin relay to the control plane. The control plane authenticates and authorizes
 * every call from the forwarded session cookie; this route adds only origin enforcement and
 * the CSRF header, so browser code never handles either credential.
 */
type Context = { params: Promise<{ path: string[] }> }

const segment = /^[A-Za-z0-9_.:@~-]{1,256}$/
/**
 * Joining a terminal with a code has no account behind it: the control plane authenticates
 * these two writes by the console origin and the terminal's own guest cookie, not a session or
 * CSRF token. Only these exact routes skip the session-backed CSRF step.
 */
const guestWrites = /^terminal-sessions\/[A-Za-z0-9_-]{1,128}\/(code-join|guest-browser-ticket)$/
const forwardedRequestHeaders = ["accept", "content-type", "idempotency-key", "if-match", "last-event-id"]
const forwardedResponseHeaders = [
  "support-reference",
  "request-id",
  "content-type",
  "cache-control",
  "etag",
  "retry-after",
  "content-disposition",
  "content-length",
]

/** The route without ids: only word segments (`terminal-sessions`) are kept, so a log line never holds a token or id. */
const routeOf = (path: string[]) => path.map((part) => (/^[a-z]+(-[a-z]+)*$/.test(part) ? part : ":id")).join("/")

/**
 * One reference per request: sent to the control plane (which records its failures under it) and returned in the
 * Support-Reference header, so what the browser shows is what the control plane and this log hold. A 5xx or this
 * relay's own refusal is logged with it.
 */
function logFailure(reference: string, status: number, method: string, path: string[], requestId?: string | null) {
  if (status < 500 && status !== 403) return
  console.error(
    JSON.stringify({
      msg: "console api failure",
      support_reference: reference,
      request_id: requestId,
      status,
      method,
      route: routeOf(path),
    }),
  )
}

async function relay(request: Request, { params }: Context): Promise<Response> {
  const { path } = await params
  const reference = `support_${crypto.randomUUID()}`
  const refuse = (status: number, code: string, message: string) => {
    logFailure(reference, status, request.method, path)
    return problem(status, code, message, reference)
  }
  if (path.some((part) => !segment.test(part) || part === "." || part === "..")) {
    return problem(404, "not_found", "Not found.", reference)
  }
  const unsafe = !["GET", "HEAD"].includes(request.method)
  if (unsafe && !isSameOrigin(request)) {
    return refuse(403, "origin_failed", "Request origin is not allowed.")
  }

  const headers = new Headers({ "support-reference": reference })
  for (const name of forwardedRequestHeaders) {
    const value = request.headers.get(name)
    if (value) headers.set(name, value)
  }
  let cookie = request.headers.get("cookie") ?? ""
  let issued: Response | undefined
  if (unsafe && guestWrites.test(path.join("/"))) {
    headers.set("origin", consoleOrigin())
  } else if (unsafe) {
    const csrf = await csrfCredentials(cookie).catch(() => null)
    if (csrf === null) return refuse(503, "unavailable", "Paperboat is temporarily unavailable.")
    if (!csrf) return problem(401, "unauthenticated", "Authentication is required.", reference)
    cookie = csrf.cookie
    issued = csrf.issued
    headers.set("x-csrf-token", csrf.token)
    headers.set("origin", consoleOrigin())
  }
  if (cookie) headers.set("cookie", cookie)

  const search = new URL(request.url).search
  let upstream: Response
  try {
    // Files stream through as they arrive (uploads to an Inbox or a shared-file preview, and Inbox
    // downloads), without the 10-second deadline or being held in memory here; other requests
    // are small JSON bodies.
    const route = path.join("/")
    const fileUpload = unsafe && /^(previews\/files|machines\/[^/]+\/inbox)$/.test(route)
    const fileDownload = request.method === "GET" && /^machines\/[^/]+\/inbox\/[^/]+\/content$/.test(route)
    // Event streams and file transfers stay open; the browser's disconnect is their only deadline.
    // Everything else also ends when the browser goes away, and has the deadline.
    const open = fileUpload || fileDownload || request.headers.get("accept") === "text/event-stream"
    upstream = await controlPlaneFetch(
      `/v1/${path.join("/")}${search}`,
      {
        method: request.method,
        headers,
        body: fileUpload ? request.body : unsafe ? await request.arrayBuffer() : undefined,
        ...(fileUpload && { duplex: "half" }),
        signal: request.signal,
      },
      { deadline: !open },
    )
  } catch (error) {
    logFailure(reference, 503, request.method, path)
    console.error(
      JSON.stringify({ msg: "control plane unreachable", support_reference: reference, cause: String(error) }),
    )
    return problem(503, "unavailable", "Paperboat is temporarily unavailable.", reference)
  }
  logFailure(reference, upstream.status, request.method, path, upstream.headers.get("request-id"))

  const responseHeaders = new Headers({ "cache-control": "no-store", "support-reference": reference })
  for (const name of forwardedResponseHeaders) {
    const value = upstream.headers.get(name)
    if (value) responseHeaders.set(name, value)
  }
  // fetch() hands over the decoded body; a compressed upstream's length would cut it short.
  if (upstream.headers.get("content-encoding")) responseHeaders.delete("content-length")
  const response = new Response(upstream.body, { status: upstream.status, headers: responseHeaders })
  if (issued) relayCookies(issued, response)
  relayCookies(upstream, response)
  return response
}

function problem(status: number, code: string, message: string, reference: string): Response {
  return Response.json(
    { error: { code, message, support_reference: reference } },
    { status, headers: { "cache-control": "no-store", "support-reference": reference } },
  )
}

export { relay as GET, relay as POST, relay as PUT, relay as PATCH, relay as DELETE }
