import "server-only"
import { withDeadline } from "@/lib/deadline"

/** Session cookies are issued by the control plane; the console only relays them. */
export const sessionCookieNames = ["__Host-pb-session", "pb-dev-session"] as const
const csrfCookieNames = ["__Host-pb-csrf", "pb-dev-csrf"] as const

const requestTimeoutMs = 10_000

function serverOrigin(): string {
  const configured = process.env.PAPERBOAT_SERVER_URL
  if (!configured) throw new Error("PAPERBOAT_SERVER_URL is required")
  const url = new URL(configured)
  if (url.protocol !== "http:" && url.protocol !== "https:") {
    throw new Error("PAPERBOAT_SERVER_URL must use http or https")
  }
  return url.origin
}

export function controlPlaneUrl(pathAndQuery: string): URL {
  return new URL(pathAndQuery, `${serverOrigin()}/`)
}

/**
 * Fetches a control-plane endpoint with a bounded deadline; a caller's signal adds cancellation and
 * keeps the deadline. Streams and file transfers pass `{ deadline: false }`: the browser's
 * disconnect (their signal) is their only end.
 */
export function controlPlaneFetch(
  pathAndQuery: string,
  init: RequestInit = {},
  { deadline = true }: { deadline?: boolean } = {},
): Promise<Response> {
  return fetch(controlPlaneUrl(pathAndQuery), {
    ...init,
    cache: "no-store",
    redirect: "manual",
    signal: deadline ? withDeadline(init.signal, requestTimeoutMs) : init.signal,
  })
}

/** The browser-facing origin. WORKOS_REDIRECT_URI pins it so Host headers cannot redirect auth. */
export function consoleOrigin(): string {
  return new URL(redirectUri()).origin
}

export function redirectUri(): string {
  const configured = process.env.WORKOS_REDIRECT_URI
  if (!configured) throw new Error("WORKOS_REDIRECT_URI is required")
  const url = new URL(configured)
  if (url.pathname !== "/auth/callback") {
    throw new Error("WORKOS_REDIRECT_URI must end in /auth/callback")
  }
  return url.toString()
}

export function isSameOrigin(request: Request): boolean {
  return request.headers.get("origin") === consoleOrigin()
}

export function relayCookies(from: Response, to: Response): void {
  for (const cookie of from.headers.getSetCookie()) to.headers.append("set-cookie", cookie)
}

export function readCookie(header: string | null, name: string): string | undefined {
  for (const part of (header ?? "").split(";")) {
    const index = part.indexOf("=")
    if (index > 0 && part.slice(0, index).trim() === name) return part.slice(index + 1).trim()
  }
}

export function readCsrfCookie(header: string | null): string | undefined {
  for (const name of csrfCookieNames) {
    const value = readCookie(header, name)
    if (value) return value
  }
}

export function cookieAttributes(path: string, maxAge: number): string {
  const secure = consoleOrigin().startsWith("https:") ? "; Secure" : ""
  return `Path=${path}; HttpOnly; SameSite=Lax; Max-Age=${maxAge}${secure}`
}

/** Accepts only an in-app absolute path, so a return destination can never leave the console. */
export function safeReturnPath(value: string | null | undefined): string | undefined {
  if (!value || value.length > 1024) return
  if (!value.startsWith("/") || value.startsWith("//") || value.startsWith("/auth/")) return
  if (/[\\\s]/.test(value) || value.includes("..")) return
  try {
    const parsed = new URL(value, "http://console.invalid")
    if (parsed.origin !== "http://console.invalid") return
    return parsed.pathname + parsed.search
  } catch {
    return
  }
}

export type CsrfCredentials = { cookie: string; token: string; issued?: Response }

/**
 * Resolves the double-submit CSRF token for an unsafe request. The browser never reads it;
 * the console attaches it only after its own same-origin check. A missing token is reissued
 * by the control plane, and `issued` carries the cookie the browser must store. Returns
 * undefined only when the session is gone; other failures throw.
 */
export async function csrfCredentials(cookie: string): Promise<CsrfCredentials | undefined> {
  const existing = readCsrfCookie(cookie)
  if (existing) return { cookie, token: existing }
  const issued = await controlPlaneFetch("/v1/auth/csrf", { headers: { cookie } })
  if (issued.status === 401) return
  if (!issued.ok) throw new Error(`CSRF token unavailable (${issued.status})`)
  const body = (await issued.json()) as { data?: { csrf_token?: string } }
  const token = body.data?.csrf_token
  if (!token) throw new Error("CSRF token missing from response")
  const fresh = issued.headers.getSetCookie().map((value) => value.split(";", 1)[0])
  return { cookie: [cookie, ...fresh].filter(Boolean).join("; "), token, issued }
}
