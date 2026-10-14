"use client"

import { useSyncExternalStore } from "react"

/**
 * "Hide my email" for screen sharing and streaming. It is a per-browser preference until the
 * control plane stores it for every client; only this module needs to change then.
 */
const key = "pb-hide-email"
const listeners = new Set<() => void>()

function read(): boolean {
  try {
    return localStorage.getItem(key) === "1"
  } catch {
    return false
  }
}

function subscribe(listener: () => void) {
  listeners.add(listener)
  const onStorage = (event: StorageEvent) => event.key === key && listener()
  window.addEventListener("storage", onStorage)
  return () => {
    listeners.delete(listener)
    window.removeEventListener("storage", onStorage)
  }
}

export function useHideEmail(): boolean {
  return useSyncExternalStore(subscribe, read, () => false)
}

export function setHideEmail(hide: boolean) {
  try {
    if (hide) localStorage.setItem(key, "1")
    else localStorage.removeItem(key)
  } catch {
    // Storage unavailable (private mode); the preference simply does not persist.
  }
  listeners.forEach((listener) => listener())
}
