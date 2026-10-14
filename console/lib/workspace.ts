"use client"

import { createContext, useContext, useSyncExternalStore } from "react"

/**
 * The selected workspace ("personal" or a team slug) lives in a cookie so it survives reloads
 * and new tabs, the way a Tailscale tailnet selection does. The control plane validates the
 * selection on every request; this is only the user's preference.
 */
const cookieName = "pb-workspace"
const personal = "personal"
const listeners = new Set<() => void>()

function read(): string {
  const match = document.cookie.match(/(?:^|;\s*)pb-workspace=([a-z0-9-]{1,63})(?:;|$)/)
  return match?.[1] ?? personal
}

export function setWorkspace(id: string) {
  const secure = location.protocol === "https:" ? "; Secure" : ""
  document.cookie = `${cookieName}=${id}; Path=/; Max-Age=31536000; SameSite=Lax${secure}`
  listeners.forEach((listener) => listener())
}

function subscribe(listener: () => void) {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

/** The workspace the server rendered for (from the same cookie), so hydration agrees with it. */
export const ServerWorkspace = createContext(personal)

export function useWorkspaceId(): string {
  const server = useContext(ServerWorkspace)
  return useSyncExternalStore(subscribe, read, () => server)
}
