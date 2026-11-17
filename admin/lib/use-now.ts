"use client"

import { useSyncExternalStore } from "react"

/**
 * The current time, refreshed every `intervalMs`, for countdowns such as "2 h left".
 * Reading Date.now() during render would give the server and each re-render different answers.
 */
type Clock = {
  now: number
  listeners: Set<() => void>
  timer?: number
  subscribe: (listener: () => void) => () => void
}
const clocks = new Map<number, Clock>()

// One clock, with one stable subscribe function, per interval: React resubscribes whenever the
// function changes, and a resubscribe that moved the time would render forever.
function clock(intervalMs: number): Clock {
  const existing = clocks.get(intervalMs)
  if (existing) return existing
  const entry: Clock = {
    now: Date.now(),
    listeners: new Set(),
    subscribe: (listener) => {
      entry.listeners.add(listener)
      if (entry.timer === undefined) {
        // A clock that sat idle catches up once; React re-renders for the changed snapshot.
        entry.now = Date.now()
        entry.timer = window.setInterval(() => {
          entry.now = Date.now()
          entry.listeners.forEach((notify) => notify())
        }, intervalMs)
      }
      return () => {
        entry.listeners.delete(listener)
        if (entry.listeners.size === 0) {
          window.clearInterval(entry.timer)
          entry.timer = undefined
        }
      }
    },
  }
  clocks.set(intervalMs, entry)
  return entry
}

export function useNow(intervalMs = 30_000): number {
  const entry = clock(intervalMs)
  return useSyncExternalStore(
    entry.subscribe,
    () => entry.now,
    () => 0,
  )
}
