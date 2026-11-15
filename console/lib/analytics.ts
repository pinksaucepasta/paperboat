"use client"

import { useCallback } from "react"
import { meQuery, useApiQuery } from "@/lib/api"
import { isAnalyticsOff } from "@/lib/privacy"

/**
 * Product analytics (PostHog), explicit events only: no autocapture, no session replay, no SDK. An
 * event carries the account's server id (pseudonymous) and a few coarse properties; never an email,
 * machine or team name, command, file name, URL or anything typed. The key and host come from the
 * build (NEXT_PUBLIC_POSTHOG_KEY, NEXT_PUBLIC_POSTHOG_HOST); without them, with Do Not Track, Global
 * Privacy Control or the Settings switch off, nothing is sent. A blocked or failing request is ignored.
 * The events are listed in NOTES.md.
 */
const apiKey = process.env.NEXT_PUBLIC_POSTHOG_KEY
const host = process.env.NEXT_PUBLIC_POSTHOG_HOST

type Properties = Record<string, string | number | boolean>

function allowed() {
  const nav = navigator as Navigator & { globalPrivacyControl?: boolean }
  return Boolean(apiKey && host) && nav.doNotTrack !== "1" && !nav.globalPrivacyControl && !isAnalyticsOff()
}

export function track(account: string, event: string, properties: Properties = {}) {
  try {
    if (!allowed()) return
    void fetch(new URL("/i/v0/e/", host), {
      method: "POST",
      keepalive: true,
      // text/plain keeps the request simple (no CORS preflight), as PostHog's own SDK sends it.
      headers: { "content-type": "text/plain" },
      body: JSON.stringify({
        api_key: apiKey,
        event,
        distinct_id: account,
        properties: { ...properties, $geoip_disable: true },
      }),
    }).catch(() => {})
  } catch {
    // Analytics never breaks the console.
  }
}

/** `track` for the signed-in account; drops the event until the account is known. */
export function useTrack() {
  const id = useApiQuery(meQuery).data?.id
  return useCallback((event: string, properties?: Properties) => id && track(id, event, properties), [id])
}
