"use client"

import { queryOptions } from "@tanstack/react-query"
import { api, idempotencyKey } from "@/lib/api"
import type { components } from "@/lib/api-schema"

type Schemas = components["schemas"]
export type Team = Schemas["TeamTeam"]
export type TeamMember = Schemas["TeamMember"]
export type TeamInvitation = Schemas["TeamInvitationSummary"]
export type TeamAction = "role" | "remove" | "leave" | "transfer" | "delete"

const minute = 60_000
const json = { "content-type": "application/json" }
const teamPath = (team: string) => `teams/${encodeURIComponent(team)}`

export const teamQuery = (team: string) =>
  queryOptions({
    queryKey: ["team", team],
    queryFn: () => api<Team>(teamPath(team)),
    staleTime: minute,
  })

/** Invitations a team has sent; the page shows the pending ones. */
export const teamInvitationsQuery = (team: string) =>
  queryOptions({
    queryKey: ["team-invitations", team],
    queryFn: async () => (await api<{ items: TeamInvitation[] }>(`${teamPath(team)}/invitations`)).items,
    staleTime: minute,
  })

/** Invitations sent to the signed-in user, from any team. */
export const myInvitationsQuery = queryOptions({
  queryKey: ["my-invitations"],
  queryFn: async () => (await api<{ items: TeamInvitation[] }>("team-invitations")).items,
  staleTime: minute,
})

export function createTeam(teamId: string) {
  return api<Team>("teams", {
    method: "POST",
    headers: json,
    body: JSON.stringify({ operation_id: idempotencyKey(), team_id: teamId }),
  })
}

/** Invites by email; the person can accept after signing up with that address. */
export function inviteMember(team: Team, email: string) {
  return api<TeamInvitation>(`${teamPath(team.team_id)}/invitations`, {
    method: "POST",
    headers: json,
    body: JSON.stringify({ operation_id: idempotencyKey(), expected_generation: team.generation, email }),
  })
}

export function cancelInvitation(team: Team, invitation: string) {
  return api<unknown>(`${teamPath(team.team_id)}/invitations/${encodeURIComponent(invitation)}/cancel`, {
    method: "POST",
    headers: json,
    body: JSON.stringify({ operation_id: idempotencyKey(), expected_generation: team.generation }),
  })
}

export function answerInvitation(invitation: string, answer: "accept" | "decline") {
  return api<unknown>(`team-invitations/${encodeURIComponent(invitation)}/${answer}`, {
    method: "POST",
    headers: json,
    body: JSON.stringify({ operation_id: idempotencyKey() }),
  })
}

/** Role change, remove, leave, transfer ownership and delete: one endpoint, fenced by generation. */
export function teamAction(
  team: Team,
  action: TeamAction,
  fields: { account_id?: string; role?: string; confirmation?: string } = {},
) {
  return api<Team>(`${teamPath(team.team_id)}/actions`, {
    method: "POST",
    headers: json,
    body: JSON.stringify({ operation_id: idempotencyKey(), expected_generation: team.generation, action, ...fields }),
  })
}

/** The server's team ID rule: lowercase letters, digits and inner dashes, 1–63. */
export const teamIdPattern = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/
