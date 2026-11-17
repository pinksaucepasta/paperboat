"use client"

import { useMemo, useState } from "react"
import type { UseQueryResult } from "@tanstack/react-query"
import { useApiQuery, type ApiError } from "@/lib/api"

export const pageSize = 50

type Page = { total?: number; total_capped?: boolean; next_after?: string; next_cursor?: string; next?: string }

/**
 * A server-paged list. `filters` (empty values dropped) are query parameters; pages are reached by the cursors
 * the server returned for the pages before, so "Previous" never guesses. Changing a filter returns to page 1.
 * The server's `total` drives the "a–b of N" line and the page controls; lists without a total show "next" only.
 */
export type CursorList<T> = UseQueryResult<T, ApiError> & { page: number; hasNext: boolean; go: (page: number) => void }

export function useCursorList<T extends Page>(path: string, filters: Record<string, string>, cursorParam: "after" | "cursor" = "after"): CursorList<T> {
  const query = useMemo(() => {
    const params = new URLSearchParams()
    for (const [key, value] of Object.entries(filters)) if (value) params.set(key, value)
    return params.toString()
  }, [filters])
  const [state, setState] = useState<{ query: string; page: number; cursors: string[] }>({ query, page: 1, cursors: [""] })
  // A new filter starts over: the old cursors belong to the old result set.
  const current = state.query === query ? state : { query, page: 1, cursors: [""] }
  const cursor = current.cursors[current.page - 1]
  const params = new URLSearchParams(query)
  params.set("limit", String(pageSize))
  if (cursor) params.set(cursorParam, cursor)
  const result = useApiQuery<T>(["list", path, query, current.page, cursor], `${path}?${params}`)
  const next = result.data?.next_after ?? result.data?.next_cursor ?? result.data?.next
  return {
    ...result,
    page: current.page,
    hasNext: Boolean(next),
    // Going forward remembers the cursor the server gave for the next page, so Previous can come back.
    go: (page: number) =>
      setState({ ...current, page, cursors: page > current.page && next ? [...current.cursors.slice(0, current.page), next] : current.cursors }),
  }
}

