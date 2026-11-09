"use client"

import { hydrate, useQueryClient, type DehydratedState } from "@tanstack/react-query"
import { useContext, useMemo } from "react"
import { ServerQueries } from "@/lib/api"
import { ServerWorkspace } from "@/lib/workspace"
import type { Viewer } from "@/lib/format"
import { ViewerTime } from "@/lib/use-time-format"

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
  viewer,
  children,
}: {
  state: DehydratedState
  workspace: string
  /** The viewer's zone and locale; a nested ServerData without one keeps its parent's. */
  viewer?: Viewer
  children: React.ReactNode
}) {
  const queryClient = useQueryClient()
  // A page's own ServerData sits inside the layout's (session): both sets were server-rendered.
  const outer = useContext(ServerQueries)
  const outerViewer = useContext(ViewerTime)
  const hashes = useMemo(() => {
    hydrate(queryClient, state)
    return new Set([...outer, ...state.queries.map((query) => query.queryHash)])
  }, [queryClient, state, outer])
  return (
    <ServerWorkspace value={workspace}>
      <ServerQueries value={hashes}>
        <ViewerTime value={viewer ?? outerViewer}>{children}</ViewerTime>
      </ServerQueries>
    </ServerWorkspace>
  )
}
