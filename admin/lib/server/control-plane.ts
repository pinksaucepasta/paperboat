import "server-only"
import { withDeadline } from "@/lib/deadline"

/** The operator session cookie is issued by the control plane; the panel only relays it. */
export const sessionCookie = "pb-operator-session"
/** The CSRF token the control plane returns at sign-in lives here, out of reach of page scripts. */
export const csrfCookie = "pb-admin-csrf"

const requestTimeoutMs = 10_000

function serverOrigin(): string {
  const configured = process.env.ADMIN_PAPERBOAT_SERVER_URL
  if (!configured) throw new Error("ADMIN_PAPERBOAT_SERVER_URL is required")
  const url = new URL(configured)
  if (url.protocol !== "http:" && url.protocol !== "https:") {
    throw new Error("ADMIN_PAPERBOAT_SERVER_URL must use http or https")
  }
  return url.origin
}

/** The panel's own browser-facing origin; it must also be a trusted browser origin of the control plane. */
export function adminOrigin(): string {
  const configured = process.env.ADMIN_ORIGIN
  if (!configured) throw new Error("ADMIN_ORIGIN is required")
  return new URL(configured).origin
}

export function controlPlaneFetch(pathAndQuery: string, init: RequestInit = {}): Promise<Response> {
  return fetch(new URL(pathAndQuery, `${serverOrigin()}/`), {
    ...init,
    cache: "no-store",
    redirect: "manual",
    signal: withDeadline(init.signal, requestTimeoutMs),
  })
}

export function isSameOrigin(request: Request): boolean {
  return request.headers.get("origin") === adminOrigin()
}

export function readCookie(header: string | null, name: string): string | undefined {
  for (const part of (header ?? "").split(";")) {
    const index = part.indexOf("=")
    if (index > 0 && part.slice(0, index).trim() === name) return part.slice(index + 1).trim()
  }
}

/** The CSRF cookie's attributes: HttpOnly, Strict, scoped to the operator session's lifetime. */
export function csrfCookieHeader(value: string, maxAgeSeconds: number): string {
  const secure = adminOrigin().startsWith("https:") ? "; Secure" : ""
  return `${csrfCookie}=${value}; Path=/; HttpOnly; SameSite=Strict; Max-Age=${maxAgeSeconds}${secure}`
}
