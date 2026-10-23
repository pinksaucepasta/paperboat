"use client"

import { useEffect, useState } from "react"

/**
 * A list's page, kept here and written to the URL (?page=) so it can be shared and survives a
 * reload. The URL is read when the list is shown (mount, or Next showing a page it kept, which
 * re-runs effects). A different `resetKey` (a new search, filter or tab) starts again at page 1.
 */
export function usePageParam(resetKey: string) {
  const [state, setState] = useState({ page: 1, key: resetKey })
  useEffect(() => {
    const page = Number(new URLSearchParams(window.location.search).get("page")) || 1
    // eslint-disable-next-line react-hooks/set-state-in-effect -- reads the URL when the list is shown
    setState((current) => ({ page, key: current.key }))
  }, [])
  const page = state.key === resetKey ? state.page : 1
  const go = (next: number) => {
    setState({ page: next, key: resetKey })
    const url = new URL(window.location.href)
    if (next > 1) url.searchParams.set("page", String(next))
    else url.searchParams.delete("page")
    window.history.replaceState(null, "", url)
  }
  return { page, go }
}
