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

/**
 * Times for rendering. The server and the browser's first (hydrating) render must produce the
 * same text, but the clock, time zone and locale differ between them; so until hydration every
 * form is the stable UTC one, and afterwards relative times tick with `useNow`. Components use
 * this instead of the format functions or Date.now() (eslint.config.mjs enforces it).
 */
export function useTimeFormat(intervalMs?: number) {
  const now = useNow(intervalMs)
  return {
    /** 0 until hydrated, then the current time (refreshed every `intervalMs`). */
    now,
    relative: (iso: string) => (now ? formatRelative(iso, now) : formatStable(iso)),
    dateTime: (iso: string) => (now ? formatDateTime(iso) : formatStable(iso)),
    date: (iso: string, style: DateStyle) => (now ? formatDate(iso, style) : formatDateStable(iso, style)),
    exact: (iso: string) => (now ? formatExact(iso) : { local: formatStable(iso), utc: formatStable(iso) }),
  }
}
