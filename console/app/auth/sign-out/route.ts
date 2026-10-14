import {
  consoleOrigin,
  controlPlaneFetch,
  csrfCredentials,
  isSameOrigin,
  relayCookies,
} from "@/lib/server/control-plane"

const workosLogout = { origin: "https://api.workos.com", pathname: "/user_management/sessions/logout" }

export async function POST(request: Request): Promise<Response> {
  if (!isSameOrigin(request)) return new Response("Invalid request origin.", { status: 403 })
  const home = new URL("/", consoleOrigin()).toString()
  try {
    const csrf = await csrfCredentials(request.headers.get("cookie") ?? "")
    // Without a CSRF token the session is already gone; there is nothing left to revoke.
    if (!csrf) return new Response(null, { status: 303, headers: { location: home } })
    const logout = await controlPlaneFetch("/v1/auth/logout", {
      method: "POST",
      headers: { cookie: csrf.cookie, origin: consoleOrigin(), "x-csrf-token": csrf.token },
    })
    if (!logout.ok) return new Response("Sign-out failed. Try again.", { status: 502 })
    const body = (await logout.json()) as { data?: { logout_url?: string } }
    const provider = body.data?.logout_url ? URL.parse(body.data.logout_url) : null
    const location =
      provider?.origin === workosLogout.origin && provider.pathname === workosLogout.pathname
        ? provider.toString()
        : home
    const response = new Response(null, { status: 303, headers: { location, "cache-control": "no-store" } })
    relayCookies(logout, response)
    return response
  } catch {
    return new Response("Sign-out is temporarily unavailable.", { status: 503 })
  }
}
