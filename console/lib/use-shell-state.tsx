"use client"

import { createContext, useCallback, useContext, useLayoutEffect, useRef, useState, type SetStateAction } from "react"

/*
 * On a client navigation Next shows the page's static shell (WithSession's Suspense fallback) while
 * the server renders. React never reconciles a fallback with the content that replaces it, so the
 * page remounts and every `useState` the user changed meanwhile is lost. This keeps what the user
 * entered in the shell and hands it to the first render of the server page. Filters and tabs live in
 * the URL instead (useUrlFilter); this is for drafts, dialogs and selections.
 */

/** Keys the user changed in this shell; null outside one. */
const InShell = createContext<Set<string> | null>(null)
const held = new Map<string, unknown>()

/** Wraps the static shell: when the server page replaces it, what it held is dropped (the page has read it by then). */
export function ShellScope({ children }: { children: React.ReactNode }) {
  const [keys] = useState(() => new Set<string>())
  // Layout-effect cleanup runs in the commit that swaps the shell for the page, after the page's first render.
  useLayoutEffect(
    () => () => {
      for (const key of keys) held.delete(key)
      keys.clear()
    },
    [keys],
  )
  return <InShell value={keys}>{children}</InShell>
}

/**
 * `useState` whose value survives the shell → server page swap. `key` names the state uniquely
 * (include any row id). Only a value the user set is carried over, never the initial one.
 */
export function useShellState<T>(key: string): [T | undefined, (next: SetStateAction<T | undefined>) => void]
export function useShellState<T>(key: string, initial: T | (() => T)): [T, (next: SetStateAction<T>) => void]
export function useShellState<T>(key: string, initial?: T | (() => T)): [T, (next: SetStateAction<T>) => void] {
  const shell = useContext(InShell)
  const [value, setValue] = useState<T>(() =>
    !shell && held.has(key)
      ? (held.get(key) as T)
      : typeof initial === "function"
        ? (initial as () => T)()
        : (initial as T),
  )
  const touched = useRef(false)
  useLayoutEffect(() => {
    if (shell && touched.current) {
      held.set(key, value)
      shell.add(key)
    }
  }, [shell, key, value])
  const set = useCallback((next: SetStateAction<T>) => {
    touched.current = true
    setValue(next)
  }, [])
  return [value, set]
}
