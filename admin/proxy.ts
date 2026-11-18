import { NextResponse, type NextRequest } from "next/server"

/**
 * Optimistic gate: a visitor without the operator session cookie goes straight to sign-in. The control plane
 * still validates the session on every API call; this only saves an empty shell.
 */
export function proxy(request: NextRequest) {
  if (request.cookies.has("pb-operator-session")) return NextResponse.next()
  const signIn = new URL("/login", process.env.ADMIN_ORIGIN ?? request.url)
  const { pathname, search } = request.nextUrl
  if (pathname !== "/") signIn.searchParams.set("return_to", pathname + search)
  return NextResponse.redirect(signIn)
}

export const config = {
  matcher: ["/((?!api/|login|_next/|icon.svg|patterns/).*)"],
}
