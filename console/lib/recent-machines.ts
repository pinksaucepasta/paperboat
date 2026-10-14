"use client"

/** The last few machines picked in each workspace, so the picker can lead with them. */
const max = 5
const key = (workspace: string) => `pb-recent-machines:${workspace}`

export function recentMachines(workspace: string): string[] {
  try {
    const value = JSON.parse(localStorage.getItem(key(workspace)) ?? "[]")
    return Array.isArray(value) ? value.filter((id): id is string => typeof id === "string").slice(0, max) : []
  } catch {
    return []
  }
}

export function rememberMachine(workspace: string, id: string) {
  try {
    const next = [id, ...recentMachines(workspace).filter((recent) => recent !== id)].slice(0, max)
    localStorage.setItem(key(workspace), JSON.stringify(next))
  } catch {
    // Storage unavailable; the picker falls back to online-first ordering.
  }
}
