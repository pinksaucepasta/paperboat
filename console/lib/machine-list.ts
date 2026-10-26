"use client"

import { keepPreviousData, queryOptions } from "@tanstack/react-query"
import { cursorPage } from "@/lib/cursor-pages"
import type { Machine } from "@/lib/api"
import { queryKeys } from "@/lib/query-keys"
import type { Filters } from "@/lib/machine-filter"

/** The search box's filters as GET /v1/machines takes them (Lane Q, PQ-04 C1). */
function serverFilters(filters: Filters): Record<string, string> {
  return {
    ...(filters.status && { state: filters.status }),
    ...(filters.os && { platform: filters.os }),
    ...(filters.tag && { tag: filters.tag }),
    ...(filters.owner && { owner: filters.owner === "me" ? "mine" : "shared" }),
  }
}

/**
 * One page of the workspace's machines in name order, searched, filtered and counted by the
 * server; only the page on screen is loaded and refreshed. Revoked machines appear only when the
 * Status filter asks for them (the server's default leaves them out).
 */
export const machineListQuery = (workspace: string, text: string, filters: Filters, page: number) =>
  queryOptions({
    queryKey: queryKeys.machineList(workspace, text, filters, page),
    queryFn: () =>
      cursorPage<Machine>("machines", { workspace, ...(text && { q: text }), ...serverFilters(filters) }, page),
    placeholderData: keepPreviousData,
    refetchInterval: 15_000,
  })
