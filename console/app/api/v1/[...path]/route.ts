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
  "content-type",
  "cache-control",
  "etag",
  "retry-after",
  "content-disposition",
  "content-length",
]

async function relay(request: Request, { params }: Context): Promise<Response> {
  const { path } = await params
  if (path.some((part) => !segment.test(part) || part === "." || part === "..")) {
    return problem(404, "not_found", "Not found.")
  }
  const unsafe = !["GET", "HEAD"].includes(request.method)
  if (unsafe && !isSameOrigin(request)) {
    return problem(403, "origin_failed", "Request origin is not allowed.")
  }

  const headers = new Headers()
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
    if (csrf === null) return problem(503, "unavailable", "Paperboat is temporarily unavailable.")
    if (!csrf) return problem(401, "unauthenticated", "Authentication is required.")
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
    upstream = await controlPlaneFetch(`/v1/${path.join("/")}${search}`, {
      method: request.method,
      headers,
      body: fileUpload ? request.body : unsafe ? await request.arrayBuffer() : undefined,
      ...(fileUpload && { duplex: "half" }),
      // Event streams and file transfers stay open; the browser's disconnect is their only deadline.
      signal:
        fileUpload || fileDownload || request.headers.get("accept") === "text/event-stream"
          ? request.signal
          : undefined,
    })
  } catch {
    return problem(503, "unavailable", "Paperboat is temporarily unavailable.")
  }

  const responseHeaders = new Headers({ "cache-control": "no-store" })
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

function problem(status: number, code: string, message: string): Response {
  return Response.json({ error: { code, message } }, { status, headers: { "cache-control": "no-store" } })
}

export { relay as GET, relay as POST, relay as PUT, relay as PATCH, relay as DELETE }
