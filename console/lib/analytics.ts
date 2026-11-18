"use client"

import { useCallback } from "react"
import { meQuery, useApiQuery } from "@/lib/api"
import { isAnalyticsOff } from "@/lib/privacy"

/**
 * Product analytics (PostHog), explicit events only: no autocapture, no session replay, no cookies, no SDK.
 * An event carries the account's server id (pseudonymous), a per-tab session id (sessionStorage), the page
 * as a path template without ids, names or a query string, the coarse browser, OS and device class, and
 * `app: "console"`; never an email, machine or team name, command, file name or anything typed. PostHog
 * derives the country from the request address and discards the address (project setting). The key and
 * host come from the build (NEXT_PUBLIC_POSTHOG_KEY, NEXT_PUBLIC_POSTHOG_HOST); without them, with Do Not
 * Track, Global Privacy Control or the Settings switch off, nothing is sent. A blocked or failing request
 * is ignored. The events are listed in NOTES.md.
 */
const apiKey = process.env.NEXT_PUBLIC_POSTHOG_KEY
const host = process.env.NEXT_PUBLIC_POSTHOG_HOST

type Properties = Record<string, string | number | boolean>

function allowed() {
  const nav = navigator as Navigator & { globalPrivacyControl?: boolean }
  return Boolean(apiKey && host) && nav.doNotTrack !== "1" && !nav.globalPrivacyControl && !isAnalyticsOff()
}

/** Coarse browser, OS and device class from the user agent, in PostHog's own names (the SDK-less API does not derive them). */
export function client(ua: string, mobile?: boolean) {
  const browser = /Edg\//.test(ua) ? "Microsoft Edge" : /OPR\/|Opera/.test(ua) ? "Opera" : /Firefox\/|FxiOS/.test(ua) ? "Firefox" : /Chrome\/|CriOS/.test(ua) ? "Chrome" : /Safari\//.test(ua) ? "Safari" : "Other"
  const os = /Windows/.test(ua) ? "Windows" : /Android/.test(ua) ? "Android" : /iPhone|iPad|iPod/.test(ua) ? "iOS" : /Mac OS X|Macintosh/.test(ua) ? "Mac OS X" : /CrOS/.test(ua) ? "Chrome OS" : /Linux|X11/.test(ua) ? "Linux" : "Other"
  const tablet = /iPad/.test(ua) || (/Android/.test(ua) && !/Mobile/.test(ua))
  return { $browser: browser, $os: os, $device_type: tablet ? "Tablet" : mobile || /Mobi|iPhone|iPod/.test(ua) ? "Mobile" : "Desktop" }
}

/** A UUIDv7 (millisecond timestamp first): PostHog's sessions need the session id to be one. */
function uuidv7() {
  const b = crypto.getRandomValues(new Uint8Array(16))
  let t = Date.now()
  for (let i = 5; i >= 0; i--) {
    b[i] = t % 256
    t = Math.floor(t / 256)
  }
  b[6] = (b[6] & 0x0f) | 0x70
  b[8] = (b[8] & 0x3f) | 0x80
  const h = Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("")
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`
}

/** One id per tab, random, gone with the tab: PostHog's `$session_id`. Storage blocked: each page is its own session. */
export function sessionId() {
  try {
    const stored = sessionStorage.getItem("pb-session")
    if (stored) return stored
    const made = uuidv7()
    sessionStorage.setItem("pb-session", made)
    return made
  } catch {
    return uuidv7()
  }
}

/** The page a route template names, e.g. `/machines/[machineId]/terminals`; never a real id, name or query. */
export function routePath(pathname: string, params: Record<string, string | string[] | undefined>) {
  const names = new Map<string, string>()
  for (const [key, value] of Object.entries(params)) {
    if (key === "tab") continue
    for (const v of [value ?? []].flat()) names.set(v, key).set(encodeURIComponent(v), key)
  }
  const decoded = (segment: string) => {
    try {
      return decodeURIComponent(segment)
    } catch {
      return segment
    }
  }
  return pathname
    .split("/")
    .map((segment) => {
      const key = names.get(segment) ?? names.get(decoded(segment))
      return key ? `[${key}]` : segment
    })
    .join("/")
}

/** Properties every event carries: the session, the page and the client class. `$current_url` is origin + route, no query. */
export function common(route: string): Properties {
  const nav = navigator as Navigator & { userAgentData?: { mobile?: boolean } }
  return { app: "console", $session_id: sessionId(), $pathname: route, $host: location.host, $current_url: location.origin + route, ...client(navigator.userAgent, nav.userAgentData?.mobile) }
}

/** The route template of the page now showing, set by the pageview hook; events outside it fall back to the raw path's first segment. */
let currentRoute = ""
export const setRoute = (route: string) => (currentRoute = route)

function send(account: string, event: string, properties: Properties) {
  try {
    if (!allowed()) return
    void fetch(new URL("/i/v0/e/", host), {
      method: "POST",
      keepalive: true,
      // text/plain keeps the request simple (no CORS preflight), as PostHog's own SDK sends it.
      headers: { "content-type": "text/plain" },
      body: JSON.stringify({ api_key: apiKey, event, distinct_id: account, properties }),
    }).catch(() => {})
  } catch {
    // Analytics never breaks the console.
  }
}

export function track(account: string, event: string, properties: Properties = {}) {
  send(account, event, { ...properties, ...common(currentRoute || "/") })
}

/**
 * A page view of `route`. `referrer` is `document.referrer` on the first page of a load (empty: direct) and
 * undefined on a client-side navigation (the referrer is then the console itself).
 */
export function pageview(account: string, route: string, referrer?: string) {
  setRoute(route)
  let domain = location.host
  if (referrer !== undefined) {
    try {
      domain = referrer ? new URL(referrer).host : "$direct"
    } catch {
      domain = "$direct"
    }
  }
  send(account, "$pageview", { ...common(route), $referring_domain: domain })
}

export function pageleave(account: string) {
  if (currentRoute) send(account, "$pageleave", common(currentRoute))
}

/** `track` for the signed-in account; drops the event until the account is known. */
export function useTrack() {
  const id = useApiQuery(meQuery).data?.id
  return useCallback((event: string, properties?: Properties) => id && track(id, event, properties), [id])
}
