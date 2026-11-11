"use client"

import { ViewTransition } from "react"

// React starts a transition for every boundary it updates. Under reduced motion none is started:
// the update runs at once and React gets a finished stand-in, so no animation, snapshot or
// pseudo-element ever appears. Read per call, so a changed setting applies without a reload.
// (Skipping a started one instead makes React report an AbortError.)
if (typeof document !== "undefined" && document.startViewTransition) {
  const start = document.startViewTransition.bind(document)
  document.startViewTransition = (options?: Parameters<typeof start>[0]) => {
    if (!matchMedia("(prefers-reduced-motion: reduce)").matches) return start(options)
    const update = typeof options === "function" ? options : options?.update
    const done = Promise.resolve().then(update)
    return {
      ready: done,
      finished: done,
      updateCallbackDone: done,
      types: new Set<string>(),
      skipTransition() {},
    } as unknown as ReturnType<typeof start>
  }
}

/**
 * A page's content area (never the sidebar or top bar): a short cross-fade when the page replaces
 * another. `default="none"` keeps refreshes, polling and typing silent (DESIGN.md, Motion and view transitions).
 */
export function PageTransition({ children }: { children: React.ReactNode }) {
  return (
    <ViewTransition default="none" enter="vt-fade" exit="vt-fade">
      {children}
    </ViewTransition>
  )
}

/** One element that exists on both sides of a navigation (a machine's name): it moves instead of cross-fading. */
export function SharedName({ name, children }: { name?: string; children: React.ReactNode }) {
  return (
    <ViewTransition name={name} default="none" share="vt-morph">
      {children}
    </ViewTransition>
  )
}
