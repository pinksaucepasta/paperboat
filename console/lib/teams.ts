"use client"

import { queryOptions } from "@tanstack/react-query"
import { api, idempotencyKey } from "@/lib/api"
import type { components } from "@/lib/api-schema"

type Schemas = components["schemas"]
/** F19 adds `directory_managed` and `sso_exempt` (until the OpenAPI document carries them). */
export type TeamMember = Schemas["TeamMember"] & { directory_managed?: boolean; sso_exempt?: boolean }
/** The server sends `suspended` but OpenAPI omits it (NOTES.md). */
export type Team = Schemas["TeamTeam"] & { suspended?: boolean }
export type TeamInvitation = Schemas["TeamInvitationSummary"]
export type MachineAdmission = Schemas["TeamMachineAdmission"]
export type TeamAction =
  "role" | "remove" | "leave" | "transfer" | "delete" | "rename" | "machine_approval" | "private_access"
export type TeamRole = "owner" | "admin" | "billing" | "auditor" | "member"

/**
 * Team features whose server side is not deployed yet; their controls render disabled.
 * `businessRoles`: billing and auditor roles, directory-managed members and SSO (F19).
 */
export const teamLive = { inviteRole: true, rename: true, mail: true, businessRoles: false } as const

/** Role names and what each is for (server/docs/teams-business.md "Roles"). */
export const roleLabels: Record<string, string> = {
  owner: "Owner",
  admin: "Admin",
  billing: "Billing admin",
  auditor: "Auditor",
  member: "Member",
}
export const roleDescriptions: Record<string, string> = {
  admin: "Runs the team: members, settings, machines and security",
  billing: "Plan, seats, invoices and billing details",
  auditor: "Sees what an admin sees, without changing it; reads the audit log",
  member: "Uses what the team gives them",
}
export const roleOrder: Record<string, number> = { owner: 0, admin: 1, billing: 2, auditor: 3, member: 4 }

/**
 * The roles `actor` may give `target` (teams-business.md): the owner gives any role but owner;
 * an admin manages only people without member management (not owners or admins) and never makes
 * an admin. Empty when the actor may not change the target's role.
 */
export function assignableRoles(actor: string, target: string): string[] {
  const all = teamLive.businessRoles ? ["admin", "billing", "auditor", "member"] : ["admin", "member"]
  if (target === "owner") return []
  if (actor === "owner") return all
  if (actor === "admin" && teamLive.businessRoles && target !== "admin") return all.filter((role) => role !== "admin")
  return []
}

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

/**
 * Team changes are fenced by the team's generation. The page's copy can be a moment behind its own
 * previous change, so a change reads the generation just before it is sent.
 */
const freshGeneration = async (team: Team) => (await api<Team>(teamPath(team.team_id))).generation

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
  // `direction=received` is the server default once the mail producer ships; until then the page
  // also filters out invitations the user sent.
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
export async function createPaidTeam(teamId: string, seats: number, interval: "month" | "year", key: string) {
  const back = `${window.location.origin}/team`
  // The server wants the operation ID and the Idempotency-Key to match; a retry of the same form
  // reuses them, so an unknown outcome cannot create two orders.
  const { url } = await api<{ url: string }>("teams", {
    method: "POST",
    headers: { ...json, "idempotency-key": key },
    body: JSON.stringify({
      operation_id: key,
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

export async function cancelInvitation(team: Team, invitation: string) {
  const expected_generation = await freshGeneration(team)
  return api<unknown>(`${teamPath(team.team_id)}/invitations/${encodeURIComponent(invitation)}/cancel`, {
    method: "POST",
    headers: json,
    body: JSON.stringify({ operation_id: idempotencyKey(), expected_generation }),
  })
}

/** Sends the email again and gives the invitation a fresh 30 days; the server allows one a minute. */
export function resendInvitation(team: Team, invitation: string) {
  return api<TeamInvitation>(`${teamPath(team.team_id)}/invitations/${encodeURIComponent(invitation)}/resend`, {
    method: "POST",
    headers: json,
    body: JSON.stringify({ operation_id: idempotencyKey() }),
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
export async function teamAction(
  team: Team,
  action: TeamAction,
  fields: {
    account_id?: string
    role?: string
    confirmation?: string
    name?: string
    approve_new_machines?: boolean
    allow_private_access?: boolean
  } = {},
) {
  const expected_generation = await freshGeneration(team)
  return api<Team>(`${teamPath(team.team_id)}/actions`, {
    method: "POST",
    headers: json,
    body: JSON.stringify({ operation_id: idempotencyKey(), expected_generation, action, ...fields }),
  })
}

/**
 * An owner or admin answers a machine a member asked to add ("approve", "decline"); its owner can
 * withdraw the request ("unshare").
 */
export async function answerMachine(team: Team, machineId: string, action: "approve" | "decline" | "unshare") {
  const expected_generation = await freshGeneration(team)
  return api<Team>(`${teamPath(team.team_id)}/machines`, {
    method: "POST",
    headers: json,
    body: JSON.stringify({
      operation_id: idempotencyKey(),
      expected_generation,
      machine_id: machineId,
      action,
    }),
  })
}

/** What a member can do on a team machine, in the order the grant dialog lists them. */
export const machineCapabilities = [
  { id: "terminal", label: "Terminal", hint: "Open terminals and see their output." },
  { id: "exec", label: "Run commands", hint: "Run a command and read its output." },
  { id: "managed_ssh", label: "SSH, SCP and SFTP", hint: "Connect with ssh and copy files with scp, sftp and rsync." },
  { id: "files", label: "Files and transfers", hint: "Send files to and receive files from the machine." },
  { id: "preview_manage", label: "Previews", hint: "Create and stop previews of its ports." },
  { id: "tunnel_manage", label: "Tunnels", hint: "Create, change and remove its tunnels." },
  {
    id: "private_access",
    label: "Private network access",
    hint: "Reach its services (ports 1024 and up) over the private network, encrypted end to end.",
  },
] as const

export type MachineCapability = (typeof machineCapabilities)[number]["id"]

/**
 * Sets what the whole team (`account` empty) or one member may do on a team machine. `capabilities`
 * is the complete set; turning access off sends the previous set with `active: false`.
 */
export async function grantMachine(
  team: Team,
  machineId: string,
  audience: { account?: string },
  capabilities: string[],
  active: boolean,
) {
  const expected_generation = await freshGeneration(team)
  return api<Team>(`${teamPath(team.team_id)}/machine-grants`, {
    method: "POST",
    headers: json,
    body: JSON.stringify({
      operation_id: idempotencyKey(),
      expected_generation,
      machine_id: machineId,
      audience: audience.account ? "selected_member" : "all_members",
      ...(audience.account ? { account_id: audience.account } : {}),
      capabilities,
      active,
    }),
  })
}

/** The server's team ID rule: lowercase letters, digits and inner dashes, 1–63. */
export const teamIdPattern = /^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$/
