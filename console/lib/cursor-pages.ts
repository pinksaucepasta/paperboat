"use client"

import { api } from "@/lib/api"

/** A cursor-paged list's page as the server sends it: `total` counts every item behind the cursor. */
type CursorPage<T> = { items: T[] | null; next_cursor?: string; total: number }

export const cursorPageSize = 50

/**
 * The cursors that start each page of a filtered list, learned while paging forward, so page N is
 * one request once page N-1 was seen. A deep link to page N walks forward from the last page known
 * (each list is bounded by its total). Keyed by path and filters: a cursor is only valid for them.
 */
const starts = new Map<string, string[]>()

export async function cursorPage<T, Raw = CursorPage<T>>(
  path: string,
  params: Record<string, string>,
  page: number,
  signal?: AbortSignal,
  /** For a list that nests its rows and total (`environments`, `pagination`) instead of `items`. */
  read: (raw: Raw) => CursorPage<T> = (raw) => raw as unknown as CursorPage<T>,
) {
  const key = `${path}?${new URLSearchParams(params)}`
  const known = starts.get(key) ?? [""]
  let at = Math.min(page, known.length) - 1
  for (;;) {
    const query = new URLSearchParams({
      ...params,
      limit: String(cursorPageSize),
      ...(known[at] && { cursor: known[at] }),
    })
    const result = read(await api<Raw>(`${path}?${query}`, { signal }))
    if (result.next_cursor) known[at + 1] = result.next_cursor
    starts.set(key, known)
    // The page asked for, or the last one when the list is shorter than it.
    if (at + 1 >= page || !result.next_cursor) return { items: result.items ?? [], total: result.total, page: at + 1 }
    at++
  }
}
