"use client"

import { useEffect, useSyncExternalStore } from "react"

/*
 * How many rows a list had the last time it loaded, per browser (a count, never data), so its
 * loading state draws that many and the page doesn't shrink or grow when the rows arrive.
 */
const prefix = "pb-rows:"
const subscribeNothing = () => () => {}

function read(key: string): number | undefined {
  try {
    const value = localStorage.getItem(prefix + key)
    return value === null ? undefined : Number(value)
  } catch {
    return undefined
  }
}

/** The count stored under `key` (undefined on the server and on a first visit); stores `current` once known. */
export function useLastCount(key: string, current: number | undefined): number | undefined {
  // The server can't know it, so hydration renders without it and the stored count follows.
  const stored = useSyncExternalStore(subscribeNothing, () => read(key), () => undefined)
  useEffect(() => {
    if (current === undefined) return
    try {
      localStorage.setItem(prefix + key, String(current))
    } catch {
      // Storage unavailable (private mode): the next load falls back to the default.
    }
  }, [key, current])
  return stored
}
