"use client"

import { queryOptions } from "@tanstack/react-query"
import { api, apiAction, type Data } from "@/lib/api"

export type CliSession = Data<"/v1/auth/cli-client-sessions">["items"][number]
export const cliSessionsPageSize = 20

/** One page of the account's active command-line sign-ins, counted by the server. */
export const cliSessionsQuery = (page: number) =>
  queryOptions({
    queryKey: ["cli-client-sessions", page] as const,
    queryFn: ({ signal }) =>
      api<Data<"/v1/auth/cli-client-sessions">>(
        `auth/cli-client-sessions?${new URLSearchParams({
          state: "active",
          limit: String(cliSessionsPageSize),
          offset: String((page - 1) * cliSessionsPageSize),
        })}`,
        { signal },
      ),
    placeholderData: (previous) => previous,
  })

export const signOutCliSession = (id: string) =>
  apiAction(`auth/cli-client-sessions/${encodeURIComponent(id)}`, { method: "DELETE" })
