"use client"

import { queryOptions } from "@tanstack/react-query"
import { api, idempotencyKey } from "@/lib/api"
import type { components } from "@/lib/api-schema"

type Schemas = components["schemas"]
/**
 * Identity fields requested from Lane B (PLAN.md Log, 2026-10-18), optional until served. Until
 * then a member's email comes from the invitation they accepted (`memberEmails`).
 */
export type Team = Schemas["TeamTeam"] & { display_name?: string }
export type TeamMember = Schemas["TeamMember"] & { email?: string; display_name?: string; joined_at?: string }
export type TeamInvitation = Schemas["TeamInvitationSummary"] & { created_by_email?: string; created_at?: string }
export type TeamAction = "role" | "remove" | "leave" | "transfer" | "delete" | "rename"
export type TeamRole = "owner" | "admin" | "member"

/** Team features whose server side is not deployed yet; their controls render disabled. */
export const teamLive = { inviteRole: false, rename: false } as const

/** Account ID → email, from members' own fields or else the invitations they accepted. */
export function memberEmails(team: Team, invitations: TeamInvitation[] | undefined) {
  const emails = new Map<string, string>()
  for (const invitation of invitations ?? [])
    if (invitation.state === "accepted" && invitation.email) emails.set(invitation.account_id, invitation.email)
  for (const member of team.members) if (member.email) emails.set(member.account_id, member.email)
  return emails
}

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

/**
 * Creating a team is paying first (NOTES.md "Teams"): the server returns Checkout for the seats,
 * and the team exists once payment succeeds. Used when `billingLive.paidTeamCreate` is on.
 */
export async function createPaidTeam(teamId: string, seats: number, interval: "month" | "year") {
  const back = `${window.location.origin}/team`
  const { url } = await api<{ url: string }>("teams", {
    method: "POST",
    headers: json,
    body: JSON.stringify({
      operation_id: idempotencyKey(),
      team_id: teamId,
      seats,
      interval,
      success_url: `${back}?created=${encodeURIComponent(teamId)}`,
      cancel_url: back,
    }),
  })
  return url
}

/** Invites by email; the person can accept after signing up with that address. */
export function inviteMember(team: Team, email: string, role: TeamRole = "member") {
  return api<TeamInvitation>(`${teamPath(team.team_id)}/invitations`, {
    method: "POST",
    headers: json,
    body: JSON.stringify({
      operation_id: idempotencyKey(),
      expected_generation: team.generation,
      email,
      ...(teamLive.inviteRole && role !== "member" ? { role } : {}),
    }),
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
  fields: { account_id?: string; role?: string; confirmation?: string; name?: string } = {},
) {
  return api<Team>(`${teamPath(team.team_id)}/actions`, {
    method: "POST",
    headers: json,
    body: JSON.stringify({ operation_id: idempotencyKey(), expected_generation: team.generation, action, ...fields }),
  })
}

/** The server's team ID rule: lowercase letters, digits and inner dashes, 1–63. */
export const teamIdPattern = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/
