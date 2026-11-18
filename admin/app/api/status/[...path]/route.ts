import { type NextRequest, NextResponse } from "next/server"
import { controlPlaneFetch, adminOrigin, isSameOrigin, readCookie, sessionCookie } from "@/lib/server/control-plane"
import { statusProvider, StatusProviderError } from "@/lib/server/status-provider"

// The panel's Status section behind the operator session. Every call goes through statusProvider, so swapping the status page
// provider does not touch this route's callers. Optional: without a connected provider the panel says so instead of failing.
const id = /^[\w-]{1,64}$/
const err = (status: number, code: string, message: string) => NextResponse.json({ error: { code, message } }, { status })

async function relay(request: NextRequest, { params }: { params: Promise<{ path: string[] }> }) {
  const { path } = await params
  const [head, a, b] = path
  const post = request.method === "POST"
  const route = !post && path.length === 1 && head === "state" ? "state"
    : post && path.length === 1 && head === "incidents" ? "createIncident"
    : post && path.length === 3 && head === "incidents" && b === "updates" && id.test(a) ? "updateIncident"
    : post && path.length === 1 && head === "maintenance" ? "createMaintenance"
    : post && path.length === 3 && head === "maintenance" && b === "cancel" && id.test(a) ? "cancelMaintenance"
    : null
  if (!route) return err(404, "not_found", "Not found.")
  if (post && !isSameOrigin(request)) return err(403, "origin_failed", "Origin is not allowed.")

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

  if (!statusProvider.configured()) {
    return route === "state" ? NextResponse.json({ data: { configured: false } }) : err(503, "status_not_configured", "The status page is not set up for this panel.")
  }
  try {
    const input = post ? await request.json().catch(() => ({})) : undefined
    const out =
      route === "state" ? { configured: true, ...((await statusProvider.state()) as object) }
      : route === "createIncident" ? await statusProvider.createIncident(input)
      : route === "updateIncident" ? await statusProvider.updateIncident(a, input)
      : route === "createMaintenance" ? await statusProvider.createMaintenance(input)
      : await statusProvider.cancelMaintenance(a)
    return NextResponse.json({ data: out }, { headers: { "cache-control": "no-store" } })
  } catch (error) {
    if (error instanceof StatusProviderError) return err(error.status, "status_failed", error.message)
    throw error
  }
}

export { relay as GET, relay as POST }
