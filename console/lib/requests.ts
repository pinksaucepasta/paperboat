"use client"

import { queryOptions } from "@tanstack/react-query"
import { api, type Data, type Workspace } from "@/lib/api"
import type { InboxRequest } from "@/lib/files"
import { queryKeys } from "@/lib/query-keys"
import type { MachineAdmission, TeamInvitation, TeamSummary } from "@/lib/teams"

/** Rows per page in the Requests list. */
export const requestsPageSize = 10

export type RequestKind = "files" | "invitations" | "machines" | "announcements"
export const requestKinds: { id: RequestKind; label: string }[] = [
  { id: "files", label: "Files" },
  { id: "invitations", label: "Invitations" },
  { id: "machines", label: "Machines" },
  { id: "announcements", label: "Announcements" },
]

/** A machine a member asked to add to a team you own or administer. */
export type MachineRequest = { team: TeamSummary; request: MachineAdmission }

type Paged<T> = { rows: T[]; total: number }
const offsetOf = (page: number) => String((page - 1) * requestsPageSize)

/** Files teammates want to send to your machines, waiting for your answer (the server pages and counts them). */
export const waitingFilesQuery = (workspace: string, page: number) =>
  queryOptions({
    queryKey: [...queryKeys.inboxRequests(workspace), "waiting", page],
    queryFn: async ({ signal }): Promise<Paged<InboxRequest>> => {
      const query = new URLSearchParams({
        workspace,
        state: "pending",
        owner: "mine",
        limit: String(requestsPageSize),
        offset: offsetOf(page),
      })
      const data = await api<Data<"/v1/team-inbox/requests">>(
        `team-inbox/requests?${query}`,
        { signal },
      )
      return { rows: data.requests, total: data.pagination.total }
    },
    refetchInterval: 30_000,
  })

/** Team invitations sent to you that are still open. */
export const waitingInvitationsQuery = (page: number) =>
  queryOptions({
    queryKey: ["my-invitations", "waiting", page],
    queryFn: async ({ signal }): Promise<Paged<TeamInvitation>> => {
      const query = new URLSearchParams({
        direction: "received",
        state: "pending",
        limit: String(requestsPageSize),
        offset: offsetOf(page),
      })
      const data = await api<Data<"/v1/team-invitations">>(
        `team-invitations?${query}`,
        { signal },
      )
      return { rows: data.items, total: data.pagination.total }
    },
    refetchInterval: 30_000,
  })

/** One team's summary with the machine requests its owners and admins can answer. */
export const teamRequestsQuery = (team: string) =>
  queryOptions({
    queryKey: ["team-summary", team],
    queryFn: ({ signal }) => api<TeamSummary>(`teams/${encodeURIComponent(team)}`, { signal }),
    refetchInterval: 30_000,
  })

/** Teams where you can answer machine requests. */
export const answersMachines = (workspace: Workspace) =>
  workspace.kind === "team" && (workspace.role === "owner" || workspace.role === "admin")

/** The pending machine requests of the teams that returned a summary. */
export const machineRequestsOf = (summaries: (TeamSummary | undefined)[]): MachineRequest[] =>
  summaries.flatMap((team) =>
    team ? (team.machine_requests ?? []).filter((request) => request.state === "pending").map((request) => ({ team, request })) : [],
  )
