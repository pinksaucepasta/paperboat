import { type NextRequest, NextResponse } from "next/server"
import {
  controlPlaneFetch,
  adminOrigin,
  csrfCookie,
  csrfCookieHeader,
  isSameOrigin,
  readCookie,
  sessionCookie,
} from "@/lib/server/control-plane"

// The panel talks to the operator and admin APIs only; everything else on the control plane stays out of reach.
const allowedRoots = new Set(["operator", "admin"])
const segment = /^[A-Za-z0-9._:~%-]{1,256}$/
const forwardedRequestHeaders = ["content-type", "accept", "idempotency-key", "operator-target", "operator-reason"]
const forwardedResponseHeaders = ["content-type", "request-id", "correlation-id", "support-reference", "retry-after"]
const sessionSeconds = 8 * 60 * 60

async function relay(request: NextRequest, { params }: { params: Promise<{ path: string[] }> }) {
  const { path } = await params
  if (path.length === 0 || !allowedRoots.has(path[0]) || !path.every((part) => segment.test(part) && part !== "..")) {
    return NextResponse.json({ error: { code: "not_found", message: "Not found." } }, { status: 404 })
  }
  const unsafe = request.method !== "GET" && request.method !== "HEAD"
  if (unsafe && !isSameOrigin(request)) {
    return NextResponse.json({ error: { code: "origin_failed", message: "Origin is not allowed." } }, { status: 403 })
  }
  const cookies = request.headers.get("cookie")
  const headers = new Headers({ origin: adminOrigin() })
  for (const name of forwardedRequestHeaders) {
    const value = request.headers.get(name)
    if (value) headers.set(name, value)
  }
  const session = readCookie(cookies, sessionCookie)
  if (session) headers.set("cookie", `${sessionCookie}=${session}`)
  const csrf = readCookie(cookies, csrfCookie)
  if (unsafe && csrf) headers.set("x-csrf-token", csrf)

  const target = `/v1/${path.map(encodeURIComponent).join("/")}${request.nextUrl.search}`
  let upstream: Response
  try {
    upstream = await controlPlaneFetch(target, {
      method: request.method,
      headers,
      body: unsafe ? await request.arrayBuffer() : undefined,
    })
  } catch {
    return NextResponse.json(
      { error: { code: "control_plane_unreachable", message: "The control plane did not answer." } },
      { status: 502 },
    )
  }

  const signIn = request.method === "POST" && (path.join("/") === "operator/login" || path.join("/") === "operator/setup")
  const signOut = request.method === "POST" && path.join("/") === "operator/logout"
  let body: BodyInit | null = upstream.status === 204 ? null : await upstream.arrayBuffer()
  let csrfToken: string | undefined
  if (signIn && upstream.ok) {
    // The CSRF token goes into an HttpOnly cookie; the page never sees it.
    const parsed = JSON.parse(new TextDecoder().decode(body as ArrayBuffer)) as { data?: { csrf_token?: string } }
    csrfToken = parsed.data?.csrf_token
    if (parsed.data) delete parsed.data.csrf_token
    body = JSON.stringify(parsed)
  }
  const response = new NextResponse(body, { status: upstream.status })
  for (const name of forwardedResponseHeaders) {
    const value = upstream.headers.get(name)
    if (value) response.headers.set(name, value)
  }
  response.headers.set("cache-control", "no-store")
  for (const cookie of upstream.headers.getSetCookie()) response.headers.append("set-cookie", cookie)
  if (csrfToken) response.headers.append("set-cookie", csrfCookieHeader(csrfToken, sessionSeconds))
  if (signOut) response.headers.append("set-cookie", csrfCookieHeader("", 0))
  return response
}

export { relay as GET, relay as POST, relay as PUT, relay as DELETE }
