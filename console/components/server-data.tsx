"use client"

import { hydrate, useQueryClient, type DehydratedState } from "@tanstack/react-query"
import { useMemo } from "react"
import { ServerQueries } from "@/lib/api"
import { ServerWorkspace } from "@/lib/workspace"

/**
 * Seeds the query cache with what the server fetched for this page, and tells `useApiQuery` it
 * may render that data during hydration (the server rendered the same).
 *
 * The data is written during render, not via `HydrationBoundary`: that defers queries already
 * in the cache to an effect, and on the server the streamed page's loading fallback has already
 * created them (empty), so the server would render skeletons while the browser renders data.
 * `hydrate` only overwrites older data, so a fresher browser result is kept.
 */
export function ServerData({
  state,
  workspace,
  children,
}: {
  state: DehydratedState
  workspace: string
  children: React.ReactNode
}) {
  const queryClient = useQueryClient()
  const hashes = useMemo(() => {
    hydrate(queryClient, state)
    return new Set(state.queries.map((query) => query.queryHash))
  }, [queryClient, state])
  return (
    <ServerWorkspace value={workspace}>
      <ServerQueries value={hashes}>{children}</ServerQueries>
    </ServerWorkspace>
  )
}
