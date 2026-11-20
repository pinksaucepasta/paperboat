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

type Caret = { selector: string; start: number | null; end: number | null; el: HTMLElement }
/** The text field the user last focused in a shell, with its caret: the swap removes that element and focus with it. */
let caret: Caret | null = null

function noteCaret() {
  const el = document.activeElement
  if (!(el instanceof HTMLInputElement || el instanceof HTMLTextAreaElement)) {
    caret = null // focus moved off the fields (a link, a button): nothing to restore
    return
  }
  // Fields are found again by a stable id, name or placeholder (React's generated ids differ per copy).
  const selector =
    el.id && !/^base-ui-|_r_/.test(el.id)
      ? `#${CSS.escape(el.id)}`
      : el.name
        ? `[name="${CSS.escape(el.name)}"]`
        : el.placeholder
          ? `[placeholder="${CSS.escape(el.placeholder)}"]`
          : null
  caret = selector ? { selector, start: el.selectionStart, end: el.selectionEnd, el } : null
}

/** After the swap commits, puts the caret back in the new copy of the field the user was typing in. */
function restoreCaret() {
  const was = caret
  caret = null
  if (!was) return
  // The page's field may appear a frame or two after the swap (an inner Suspense), so look for it for a short while.
  let frames = 10
  const look = () => {
    // Only when the shell's field is gone and the user has not moved focus elsewhere.
    if (was.el.isConnected || (document.activeElement && document.activeElement !== document.body)) return
    // Next keeps visited pages mounted but hidden: take the visible copy.
    const el = [...document.querySelectorAll<HTMLInputElement | HTMLTextAreaElement>(was.selector)].find((field) =>
      field.checkVisibility(),
    )
    if (!el) {
      if (--frames > 0) requestAnimationFrame(look)
      return
    }
    el.focus({ preventScroll: true })
    if (was.start !== null && was.end !== null) el.setSelectionRange(was.start, was.end)
  }
  requestAnimationFrame(look)
}

/** Wraps the static shell: when the server page replaces it, what it held is dropped (the page has read it by then). */
export function ShellScope({ children }: { children: React.ReactNode }) {
  const [keys] = useState(() => new Set<string>())
  // Layout-effect cleanup runs in the commit that swaps the shell for the page, after the page's first render.
  useLayoutEffect(() => {
    const events = ["focusin", "input", "keyup", "pointerup"] as const
    events.forEach((name) => document.addEventListener(name, noteCaret))
    return () => {
      events.forEach((name) => document.removeEventListener(name, noteCaret))
      for (const key of keys) held.delete(key)
      keys.clear()
      restoreCaret()
    }
  }, [keys])
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
  // A shell inside a shell (the machine layout's, then the page's) adopts too, and holds the value on for the next swap.
  const adopt = held.has(key)
  const [value, setValue] = useState<T>(() =>
    adopt ? (held.get(key) as T) : typeof initial === "function" ? (initial as () => T)() : (initial as T),
  )
  const touched = useRef(adopt)
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
