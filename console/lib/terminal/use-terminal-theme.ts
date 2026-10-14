"use client"

import { useTheme } from "next-themes"
import { useCallback, useSyncExternalStore } from "react"
import { resolveTerminalTheme } from "@/lib/terminal/themes"

/** The chosen terminal theme is a per-browser convenience, so it lives in localStorage. */
const storageKey = "paperboat:terminal-theme"
const listeners = new Set<() => void>()
// Used when storage is blocked, so a choice still applies until the page reloads.
let memory: string | null = null

function read(): string | null {
  try {
    return localStorage.getItem(storageKey) ?? memory
  } catch {
    return memory
  }
}

function subscribe(listener: () => void) {
  listeners.add(listener)
  const onStorage = (event: StorageEvent) => event.key === storageKey && listener()
  window.addEventListener("storage", onStorage)
  return () => {
    listeners.delete(listener)
    window.removeEventListener("storage", onStorage)
  }
}

export function useTerminalTheme() {
  const choice = useSyncExternalStore(subscribe, read, () => null)
  const { resolvedTheme } = useTheme()
  const theme = resolveTerminalTheme(choice, resolvedTheme === "light" ? "light" : "dark")
  const choose = useCallback((id: string) => {
    memory = id
    try {
      localStorage.setItem(storageKey, id)
    } catch {
      // Blocked storage: `memory` carries the choice.
    }
    listeners.forEach((listener) => listener())
  }, [])
  return { theme, choose }
}
