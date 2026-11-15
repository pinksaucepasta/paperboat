"use client"

import { useSyncExternalStore } from "react"

/**
 * Per-browser privacy preferences, until the control plane stores them for every client; only this
 * module needs to change then. Each is a flag in localStorage, false when storage is unavailable.
 */
function browserFlag(key: string) {
  const listeners = new Set<() => void>()
  const read = () => {
    try {
      return localStorage.getItem(key) === "1"
    } catch {
      return false
    }
  }
  const subscribe = (listener: () => void) => {
    listeners.add(listener)
    const onStorage = (event: StorageEvent) => event.key === key && listener()
    window.addEventListener("storage", onStorage)
    return () => {
      listeners.delete(listener)
      window.removeEventListener("storage", onStorage)
    }
  }
  return {
    read,
    use: () => useSyncExternalStore(subscribe, read, () => false),
    set(on: boolean) {
      try {
        if (on) localStorage.setItem(key, "1")
        else localStorage.removeItem(key)
      } catch {
        // Storage unavailable (private mode); the preference simply does not persist.
      }
      listeners.forEach((listener) => listener())
    },
  }
}

/** "Hide my email" for screen sharing and streaming. */
const hideEmail = browserFlag("pb-hide-email")
export const useHideEmail = hideEmail.use
export const setHideEmail = hideEmail.set

/** "Share usage statistics" switched off (lib/analytics.ts sends nothing then). */
const analyticsOff = browserFlag("pb-analytics-off")
export const isAnalyticsOff = analyticsOff.read
export const useAnalyticsOff = analyticsOff.use
export const setAnalyticsOff = analyticsOff.set
