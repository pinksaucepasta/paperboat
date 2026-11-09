import { NextResponse, type NextRequest } from "next/server"
import { consoleOrigin, sessionCookieNames } from "@/lib/server/control-plane"

/**
 * Optimistic gate: a visitor without any session cookie goes straight to sign-in instead of
 * seeing an empty console. The control plane still validates the session on every API call.
 */
export function proxy(request: NextRequest) {
  if (sessionCookieNames.some((name) => request.cookies.has(name))) {
    // The page's path, for sending an ended session back here after sign-in (WithSession).
    const headers = new Headers(request.headers)
    headers.set("x-pb-path", request.nextUrl.pathname + request.nextUrl.search)
    return NextResponse.next({ request: { headers } })
  }
  const signIn = new URL("/auth/sign-in", consoleOrigin())
  const { pathname, search } = request.nextUrl
  if (pathname !== "/") signIn.searchParams.set("return_to", pathname + search)
  return NextResponse.redirect(signIn)
}

export const config = {
  matcher: ["/((?!api/|auth/|_next/|patterns/|terminal/|icon.svg).*)"],
}
