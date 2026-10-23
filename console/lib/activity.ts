"use client"

import { infiniteQueryOptions } from "@tanstack/react-query"
import { api } from "@/lib/api"

/*
 * Activity: what happened in the workspace, newest first, from `GET /v1/activity`
 * (the server's audit events).
 */

export type ActivityEvent = {
  id: string
  occurred_at: string
  /** The audit event type, e.g. `auth.login`, `tunnel.created`, `user_machine.renamed`. */
  action: string
  /**
   * `api_token`: a person acting through one of their API tokens (`token_id`); `directory`: the
   * company directory (SSO directory sync); `operator`: Paperboat support.
   */
  actor: {
    kind: "user" | "api_token" | "directory" | "operator" | "machine" | "system"
    id: string
    name?: string
    email?: string
    token_id?: string
  }
  /** `denied` with a stable `reason` code for refused requests (F19 audit log). */
  result?: "success" | "denied"
  reason?: string
  ip?: string
  request_id?: string
  /** What it happened to; its name as it was then. */
  target?: { kind: string; id: string; name?: string }
  machine?: { id: string; name: string }
  /** Where it came from: the console, the CLI, a machine. */
  source?: "console" | "cli" | "machine" | "api"
}

export const activityAreas = {
  all: "All activity",
  machine: "Machines",
  terminal: "Terminals",
  publish: "Previews and tunnels",
  file: "Files",
  environment: "Environment",
  team: "Team and sharing",
  account: "Sign-ins",
} as const
export type ActivityArea = keyof typeof activityAreas

const areaActions: Record<Exclude<ActivityArea, "all">, string> = {
  machine: "machine",
  terminal: "terminal",
  publish: "preview,tunnel,domain",
  file: "file",
  environment: "environment,config",
  team: "team,share",
  account: "account",
}

/** The audit log's filters and export (F19); off until that server is deployed. */
export const activityLive = { filters: true, export: true, streaming: true } as const

export const activityPeriods = { all: "Any time", day: "Last 24 hours", week: "Last 7 days", month: "Last 30 days" } as const
export type ActivityPeriod = keyof typeof activityPeriods
const periodMs: Record<ActivityPeriod, number> = { all: 0, day: 86_400_000, week: 604_800_000, month: 2_592_000_000 }

/** The start of a period chosen at `now`; fixed when chosen, so the list doesn't refetch as time passes. */
export const periodStart = (period: ActivityPeriod, now: number) =>
  period === "all" || !now ? undefined : new Date(now - periodMs[period]).toISOString()

export type ActivityFilter = {
  workspace: string
  machineId?: string | null
  area: ActivityArea
  q: string
  /** Only refused requests. */
  denied?: boolean
  /** RFC 3339; from `periodStart`. */
  since?: string
}

/** The query string for a filter, shared by the list and the export. */
export function activityParams(input: ActivityFilter): URLSearchParams {
  const params = new URLSearchParams({ workspace: input.workspace })
  if (input.machineId) params.set("machine_id", input.machineId)
  if (input.area !== "all") params.set("area", areaActions[input.area])
  if (input.q) params.set("q", input.q)
  if (activityLive.filters && input.denied) params.set("result", "denied")
  if (activityLive.filters && input.since) params.set("since", input.since)
  return params
}

/** The download address of the filtered audit log (team workspaces; owners, admins, auditors). */
export function activityExportHref(input: ActivityFilter, format: "csv" | "json") {
  const params = activityParams(input)
  params.set("format", format)
  return `/api/v1/activity/export?${params}`
}

export const activityQuery = (input: ActivityFilter) =>
  infiniteQueryOptions({
    queryKey: ["activity", activityParams(input).toString()] as const,
    queryFn: ({ pageParam }) => {
      const params = activityParams(input)
      params.set("limit", "50")
      if (pageParam) params.set("cursor", pageParam)
      return api<{ items: ActivityEvent[]; next_cursor?: string }>(`activity?${params}`)
    },
    initialPageParam: "",
    getNextPageParam: (page) => page.next_cursor || undefined,
    retry: 1,
  })

/** What happened, as the middle of a sentence. Keys are the server's audit event types. */
const phrases: Record<string, string> = {
  "auth.login": "signed in",
  "auth.logout": "signed out",
  "auth.client.approved": "approved a CLI sign-in",
  "auth.client.revoked": "signed out a CLI",
  "auth.device.completed": "signed in on a device",
  "user_machine.enrollment_started": "started adding a machine",
  "user_machine.enrollment_approved": "added the machine",
  "user_machine.enrollment_cancelled": "cancelled adding a machine",
  "user_machine.enrollment_restarted": "restarted adding a machine",
  "user_machine.installation_failed": "couldn't finish installing",
  "user_machine.installation_retried": "retried installing",
  "user_machine.renamed": "renamed the machine",
  "user_machine.deleted": "removed the machine",
  "user_machine.disconnected": "disconnected",
  "user_machine.denied": "denied the machine",
  "user_machine.availability_policy_updated": "changed sleep settings on",
  "user_machine.capabilities_updated": "changed what's allowed on",
  "user_machine.maintenance_approval_requested": "asked to update",
  "user_machine.maintenance_approval_decided": "answered an update request for",
  "terminal.browser_access_policy_changed": "changed browser access to",
  "terminal.code_access_changed": "changed join-code access to",
  "preview.created": "started the preview",
  "preview.ready": "published the preview",
  "preview.stopped": "stopped the preview",
  "preview.expired": "let the preview expire:",
  "preview.failed": "couldn't start the preview",
  "tunnel.created": "created the tunnel",
  "tunnel.deleted": "deleted the tunnel",
  "tunnel.resumed": "resumed the tunnel",
  "tunnel.connector_revoked": "removed a connector from",
  "tunnel.connector_draining": "is replacing the connector of",
  "tunnel.connector_credential_rotation_requested": "renewed the connector credential of",
  "tunnel.domain_verification_requested": "started verifying a domain on",
  "preview.domain.created": "added the preview domain",
  "preview.domain.deleted": "removed the preview domain",
  "preview.domain.verification_requested": "started verifying the preview domain",
  "preview.dispatch_uncertain": "couldn't confirm the preview",
  "auth.device.requested": "asked to add a computer",
  "user_machine.replaced": "replaced the machine",
  "config.pull_revision_approved": "approved a config update on",
  "config.conflict_resolution_requested": "chose how to resolve a config conflict on",
  "config.credential_issued": "received config repository credentials for",
  "config.repository_access_issued": "received config repository access for",
  "managed_ssh.host_key_set_observed": "saw new SSH host keys on",
  "managed_ssh.host_key_set_promoted": "accepted new SSH host keys on",
  "managed_ssh.machine_target_registered": "registered SSH on",
  "managed_ssh.machine_target_reenrolled": "registered SSH again on",
  "managed_ssh.machine_target_port_updated": "changed the SSH port on",
  "tunnel.expired": "let the tunnel expire:",
  "tunnel.connector_enrollment_issued": "prepared a connector for",
  "tunnel.connector_enrolled": "connected a connector to",
  "preview.owner_lost": "lost the session that owns the preview",
  "preview.lease_renewed": "renewed the preview",
  "config.status_recorded": "reported config sync status for",
  "tunnel.updated": "updated the tunnel",
  "tunnel.paused": "paused the tunnel",
  "tunnel.route_created": "added a route to",
  "tunnel.route_updated": "changed a route on",
  "tunnel.route_deleted": "removed a route from",
  "tunnel.domain_created": "added a domain to",
  "tunnel.domain_deleted": "removed a domain from",
  "domain.created": "added the domain",
  "domain.deleted": "removed the domain",
  "domain.verification_requested": "started verifying",
  "environment_manifest.set": "changed environment variables",
  "config.repository_connected": "connected the config repository",
  "config.repository_disconnected": "disconnected the config repository",
  "config.conflict_resolution_applied": "resolved a config conflict",
  "config.assignment_set": "assigned config to",
  "config.assignment_cleared": "removed config from",
  "transfer_destination.default_set": "changed the Inbox folder on",
  "transfer_destination.default_cleared": "reset the Inbox folder on",
  "managed_ssh.client_key_registered": "added an SSH key",
  "managed_ssh.client_key_revoked": "removed an SSH key",
  "github.oauth.connected": "connected GitHub",
  "team.created": "created the team",
  "team.invite": "invited someone to the team",
  "team.cancel_invite": "cancelled a team invitation",
  "team.role": "changed a member's role",
  "team.rename": "renamed the team",
  "team.accept": "joined the team",
  "team.leave": "left the team",
  "team.remove": "removed someone from the team",
  "team.transfer": "transferred ownership of the team",
  "team.invitation_declined": "declined an invitation to the team",
  "team.terminal_session_grant": "shared a terminal with the team",
  "team.terminal_session_end": "ended a terminal shared with the team",
  "team.terminal_session_access_authorized": "joined a terminal shared with the team",
  "team.grant": "granted team access",
  "team.machine_share": "shared a machine with the team",
  "team.machine_unshare": "stopped sharing a machine with the team",
  "team.machine_request": "asked to add a machine to the team",
  "team.machine_approve": "accepted a machine into the team",
  "team.machine_decline": "declined a machine for the team",
  "team.machine_approval": "changed whether new machines need approval",
  "team.private_access": "changed whether members may use private access",
  "team.machine_grant": "gave a member access to a machine",
  "team.inbox_request": "asked to send files",
  "team.inbox_decide": "answered a file request",
  "team.inbox_consume": "received a file",
  "team.inbox_complete": "finished a file transfer",
  "team.config_default_changed": "changed the team's config default",
  "team.config_default_adopted": "adopted the team's config default",
  "team.config_default_unadopted": "stopped using the team's config default",
  "team.config_default_deleted": "removed the team's config default",
  "machine.access_started": "opened access to",
  "machine.access_denied": "was refused access to",
  "team.sso_required": "changed whether the team requires SSO",
  "team.jit_provisioned": "joined the team through SSO",
  "team.activity_exported": "exported the audit log",
  "billing.details_updated": "updated the billing details",
  "billing.pay_by_invoice_changed": "changed whether the team pays by invoice",
  "team.env.initialize": "set up the team's environment",
  "team.env.key_grant": "gave a member access to the team's environment",
  "team.env.rotate": "rotated the team's environment key",
  "team.env.reset": "reset the team's environment",
  "env.initialize": "set up environment variables",
  "env.key_grant": "gave a device access to environment variables",
  "env.rotate": "rotated the environment key",
  "env.reset": "reset environment variables",
  "env.write": "changed environment variables",
  "config.consent_accepted": "allowed config sync on",
  "config.consent_removed": "stopped config sync on",
  "transfer_destination.session_set": "changed the Inbox folder for a session on",
  "transfer_destination.session_cleared": "reset the Inbox folder for a session on",
}

/** Unknown types read as their verb part: `user_machine.foo_bar` → "did foo bar". */
/** Families of actions named by prefix (the server adds members to them over time). */
const prefixPhrases: [string, string][] = [
  ["team.sso_", "changed the team's SSO settings"],
  ["team.directory_", "changed the team's directory sync"],
]

export function describe(event: ActivityEvent): string {
  return (
    phrases[event.action] ??
    prefixPhrases.find(([prefix]) => event.action.startsWith(prefix))?.[1] ??
    `did ${event.action.split(".").slice(1).join(" ").replace(/_/g, " ")}`
  )
}

export function actorName(event: ActivityEvent, meId?: string, memberNames?: ReadonlyMap<string, string | undefined>) {
  if (event.actor.kind === "user" && event.actor.id === meId) return "You"
  if (event.actor.kind === "system") return "Paperboat"
  if (event.actor.kind === "directory") return "Directory sync"
  if (event.actor.kind === "operator") return "Paperboat support"
  return (
    event.actor.name ??
    event.actor.email ??
    memberNames?.get(event.actor.id) ??
    (event.actor.kind === "machine" ? "A machine" : "Someone")
  )
}

/** "through an API token" for events done with one. */
export const viaToken = (event: ActivityEvent) => event.actor.kind === "api_token" || Boolean(event.actor.token_id)

/** Where a target opens in the console, when it has a page. */
export function targetHref(event: ActivityEvent): string | undefined {
  const target = event.target
  if (!target) return
  if (target.kind === "machine") return `/machines/${encodeURIComponent(target.id)}`
  if (target.kind === "tunnel") return `/tunnels/${encodeURIComponent(target.id)}`
  if (target.kind === "terminal_session") return `/terminals/${encodeURIComponent(target.id)}`
}
