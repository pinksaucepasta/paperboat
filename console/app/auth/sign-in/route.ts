import {
  consoleOrigin,
  controlPlaneFetch,
  cookieAttributes,
  redirectUri,
  relayCookies,
  safeReturnPath,
} from "@/lib/server/control-plane"
import { connection } from "next/server"

export async function GET(request: Request): Promise<Response> {
  // Every sign-in needs a fresh WorkOS state; never prerender this redirect.
  await connection()
  const clientId = process.env.WORKOS_CLIENT_ID
  if (!clientId) return failed()
  try {
    const stateResponse = await controlPlaneFetch("/v1/auth/workos/state")
    const body = stateResponse.ok ? ((await stateResponse.json()) as { data?: { state?: string } }) : undefined
    const state = body?.data?.state
    if (!state) return failed()

    const authorize = new URL("https://api.workos.com/user_management/authorize")
    authorize.searchParams.set("client_id", clientId)
    authorize.searchParams.set("redirect_uri", redirectUri())
    authorize.searchParams.set("response_type", "code")
    authorize.searchParams.set("provider", "authkit")
    authorize.searchParams.set("state", state)

    const response = new Response(null, { status: 302, headers: { location: authorize.toString() } })
    relayCookies(stateResponse, response)
    const returnTo = safeReturnPath(new URL(request.url).searchParams.get("return_to"))
    response.headers.append(
      "set-cookie",
      returnTo
        ? `pb-return=${encodeURIComponent(returnTo)}; ${cookieAttributes("/auth", 600)}`
        : `pb-return=; ${cookieAttributes("/auth", 0)}`,
    )
    response.headers.set("cache-control", "no-store")
    return response
  } catch {
    return failed()
  }
}

function failed(): Response {
  return Response.redirect(new URL("/auth/failed", consoleOrigin()), 302)
}
