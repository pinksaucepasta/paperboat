import {
  consoleOrigin,
  controlPlaneFetch,
  cookieAttributes,
  readCookie,
  redirectUri,
  relayCookies,
  safeReturnPath,
} from "@/lib/server/control-plane"

export async function GET(request: Request): Promise<Response> {
  const params = new URL(request.url).searchParams
  const code = params.get("code")
  const state = params.get("state")
  if (!code || !state) return redirect("/auth/failed")

  const cookie = request.headers.get("cookie")
  let callback: Response
  try {
    callback = await controlPlaneFetch("/v1/auth/workos/callback", {
      method: "POST",
      headers: {
        "content-type": "application/json",
        cookie: cookie ?? "",
        origin: consoleOrigin(),
      },
      body: JSON.stringify({ code, state, redirect_uri: redirectUri() }),
    })
  } catch {
    return redirect("/auth/failed")
  }

  const stored = readCookie(cookie, "pb-return")
  const returnTo = (stored && safeReturnPath(decodeURIComponent(stored))) || "/"
  const response = redirect(callback.ok ? returnTo : await failurePath(callback))
  relayCookies(callback, response)
  response.headers.append("set-cookie", `pb-return=; ${cookieAttributes("/auth", 0)}`)
  response.headers.set("referrer-policy", "no-referrer")
  return response
}

/** A sign-in the server refused on purpose (a banned account, a blocked network) says so, with the support reference. */
async function failurePath(callback: Response): Promise<string> {
  const body = (await callback.clone().json().catch(() => null)) as { error?: { code?: unknown; support_reference?: unknown } } | null
  const code = body?.error?.code
  if (code !== "account_banned" && code !== "ip_blocked") return "/auth/failed"
  const query = new URLSearchParams({ reason: code })
  const reference = body?.error?.support_reference
  if (typeof reference === "string" && /^[\w.-]{1,80}$/.test(reference)) query.set("ref", reference)
  return `/auth/failed?${query}`
}

function redirect(path: string): Response {
  return new Response(null, {
    status: 302,
    headers: { location: new URL(path, consoleOrigin()).toString(), "cache-control": "no-store" },
  })
}
