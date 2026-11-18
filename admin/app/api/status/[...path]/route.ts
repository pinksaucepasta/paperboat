import { type NextRequest, NextResponse } from "next/server"
import { controlPlaneFetch, adminOrigin, isSameOrigin, readCookie, sessionCookie } from "@/lib/server/control-plane"

// The status page's admin API (status/worker) behind the operator session. Optional: without STATUS_API_URL and
// STATUS_ADMIN_TOKEN the panel says so and every call answers "not configured" instead of failing.
const allowed = [/^state$/, /^incidents$/, /^incidents\/[\w-]{1,64}\/updates$/, /^maintenance$/, /^maintenance\/[\w-]{1,64}\/cancel$/]
const err = (status: number, code: string, message: string) => NextResponse.json({ error: { code, message } }, { status })

async function relay(request: NextRequest, { params }: { params: Promise<{ path: string[] }> }) {
  const { path } = await params
  const sub = path.join("/")
  if (!allowed.some((re) => re.test(sub))) return err(404, "not_found", "Not found.")
  const get = request.method === "GET"
  if (get !== (sub === "state")) return err(405, "method_not_allowed", "Method not allowed.")
  if (!get && !isSameOrigin(request)) return err(403, "origin_failed", "Origin is not allowed.")

  const session = readCookie(request.headers.get("cookie"), sessionCookie)
  if (!session) return err(401, "unauthenticated", "Sign in again.")
  let me: Response
  try {
    me = await controlPlaneFetch("/v1/operator/me", { headers: { origin: adminOrigin(), cookie: `${sessionCookie}=${session}` } })
  } catch {
    return err(502, "control_plane_unreachable", "The control plane did not answer.")
  }
  const role = me.ok ? ((await me.json()) as { data?: { role?: string } }).data?.role : undefined
  if (!role) return err(401, "unauthenticated", "Sign in again.")
  if (role !== "admin" && role !== "operations") return err(403, "forbidden", "This role cannot change the status page.")

  const base = process.env.STATUS_API_URL
  const token = process.env.STATUS_ADMIN_TOKEN
  if (!base || !token) {
    return get ? NextResponse.json({ data: { configured: false } }) : err(503, "status_not_configured", "The status page is not set up for this panel.")
  }
  try {
    const upstream = await fetch(new URL(`/api/admin/${sub}`, base), {
      method: request.method,
      headers: { authorization: `Bearer ${token}`, "content-type": "application/json" },
      body: get ? undefined : await request.text(),
      cache: "no-store",
      signal: AbortSignal.timeout(15_000),
    })
    const body = (await upstream.json().catch(() => ({}))) as { error?: string }
    if (!upstream.ok) return err(upstream.status === 401 ? 502 : upstream.status, "status_failed", body.error ?? `The status service answered ${upstream.status}.`)
    return NextResponse.json({ data: get ? { configured: true, ...body } : body }, { headers: { "cache-control": "no-store" } })
  } catch {
    return err(502, "status_unreachable", "The status service did not answer.")
  }
}

export { relay as GET, relay as POST }
