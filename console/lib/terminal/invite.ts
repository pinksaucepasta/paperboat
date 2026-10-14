"use client"

import { useSyncExternalStore } from "react"
import type { CodeAccess } from "@/lib/terminal/queries"
import type { TerminalSession } from "@/lib/terminal/use-terminal-connection"

/**
 * The server returns a join code only in the response that creates it; it can't be read back
 * (NOTES.md: Terminals). So the owner's browser keeps it until it expires, like a meeting app
 * remembers its own meeting code. Only this origin can read it, and an expired or replaced
 * code is dropped. A different browser sees "New code" instead.
 */
type Saved = { code: string; generation: number; expiresAt?: string }
const prefix = "paperboat:terminal-code:"
const listeners = new Set<() => void>()
const memory = new Map<string, Saved>()
// useSyncExternalStore needs the same object back while nothing changed.
const parsed = new Map<string, { raw: string | null; value: Saved | undefined }>()

function read(sessionId: string): Saved | undefined {
  let raw: string | null = null
  try {
    raw = localStorage.getItem(prefix + sessionId)
  } catch {
    return memory.get(sessionId)
  }
  const cached = parsed.get(sessionId)
  if (cached && cached.raw === raw) return cached.value
  let value: Saved | undefined
  try {
    value = raw ? (JSON.parse(raw) as Saved) : memory.get(sessionId)
  } catch {
    value = undefined
  }
  parsed.set(sessionId, { raw, value })
  return value
}

export function rememberCode(sessionId: string, code: string, generation: number, expiresAt?: string) {
  const saved: Saved = { code, generation, expiresAt }
  memory.set(sessionId, saved)
  try {
    localStorage.setItem(prefix + sessionId, JSON.stringify(saved))
  } catch {
    // Blocked storage: this tab's memory still has it.
  }
  listeners.forEach((listener) => listener())
}

function forget(sessionId: string) {
  memory.delete(sessionId)
  try {
    localStorage.removeItem(prefix + sessionId)
  } catch {
    // Nothing stored.
  }
}

function subscribe(listener: () => void) {
  listeners.add(listener)
  const onStorage = (event: StorageEvent) => event.key?.startsWith(prefix) && listener()
  window.addEventListener("storage", onStorage)
  return () => {
    listeners.delete(listener)
    window.removeEventListener("storage", onStorage)
  }
}

/** The live code for this terminal, if this browser made it. */
export function useKnownCode(sessionId: string, access: CodeAccess | undefined): string | undefined {
  const saved = useSyncExternalStore(
    subscribe,
    () => read(sessionId),
    () => undefined,
  )
  if (!saved || !access) return undefined
  if (!access.enabled || saved.generation !== access.generation) {
    // Replaced or turned off on the server (either bumps its generation): never offer it again.
    // An older generation here is just a render that hasn't seen the new code yet.
    if (access.generation > saved.generation) queueMicrotask(() => forget(sessionId))
    return undefined
  }
  return saved.code
}

/**
 * The terminal's link. A link sent with a join code says so (`?join=code`), so the page asks for
 * the code first; a plain link asks people to sign in. The hint only picks the first screen: the
 * server decides access either way and never reveals whether a terminal takes codes.
 */
export function terminalLink(sessionId: string, withCode = false) {
  return `${window.location.origin}/terminal/${encodeURIComponent(sessionId)}${withCode ? "?join=code" : ""}`
}

/** The text "Copy invite" puts on the clipboard, ready to paste into a chat. */
export function inviteText(session: TerminalSession, code?: string, expiresAt?: string) {
  const lines = [
    "Join my terminal on Paperboat",
    `${session.name} on ${session.target.name}`,
    terminalLink(session.id, Boolean(code)),
  ]
  if (code) {
    lines.push(`Code: ${code}`)
    if (expiresAt) {
      const time = new Intl.DateTimeFormat(undefined, { timeStyle: "short" }).format(new Date(expiresAt))
      lines.push(`Works until ${time}`)
    }
  }
  return lines.join("\n")
}
