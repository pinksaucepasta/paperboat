"use client"

import {
  formatDate,
  formatDateStable,
  formatDateTime,
  formatExact,
  formatRelative,
  formatStable,
  type DateStyle,
} from "@/lib/format"
import { useNow } from "@/lib/use-now"
import { createContext, useContext } from "react"
import type { Viewer } from "@/lib/format"

/** The viewer's zone and locale as the server read them (see ServerData); absent in a static shell. */
export const ViewerTime = createContext<Viewer | undefined>(undefined)

/**
 * Times for rendering. The server and the browser's first (hydrating) render must produce the
 * same text, but the clock, time zone and locale differ between them; so until hydration every
 * form is the stable UTC one, and afterwards relative times tick with `useNow`. Components use
 * this instead of the format functions or Date.now() (eslint.config.mjs enforces it).
 */
export function useTimeFormat(intervalMs?: number) {
  const now = useNow(intervalMs)
  // With the viewer's zone and locale known, the hydrating render reads the same as the server's.
  const viewer = useContext(ViewerTime)
  return {
    /** 0 until hydrated, then the current time (refreshed every `intervalMs`). */
    now,
    relative: (iso: string) => (now ? formatRelative(iso, now) : formatStable(iso)),
    // Once hydrated the browser's own zone and locale win: the cookie is what the server saw, which is UTC on a
    // first visit and the old zone after the viewer travelled (X-27).
    dateTime: (iso: string) => (now ? formatDateTime(iso) : viewer ? formatDateTime(iso, viewer) : formatStable(iso)),
    date: (iso: string, style: DateStyle) =>
      now ? formatDate(iso, style) : viewer ? formatDate(iso, style, viewer) : formatDateStable(iso, style),
    exact: (iso: string) => (now ? formatExact(iso) : { local: formatStable(iso), utc: formatStable(iso) }),
  }
}
