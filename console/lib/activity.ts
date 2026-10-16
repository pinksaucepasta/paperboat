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
  actor: { kind: "user" | "machine" | "system"; id: string; name?: string; email?: string }
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

export const activityQuery = (input: { workspace: string; machineId?: string | null; area: ActivityArea; q: string }) =>
  infiniteQueryOptions({
    queryKey: ["activity", input.workspace, input.machineId ?? "", input.area, input.q] as const,
    queryFn: ({ pageParam }) => {
      const params = new URLSearchParams({ workspace: input.workspace, limit: "50" })
      if (input.machineId) params.set("machine_id", input.machineId)
      if (input.area !== "all") params.set("area", areaActions[input.area])
      if (input.q) params.set("q", input.q)
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
}

/** Unknown types read as their verb part: `user_machine.foo_bar` → "foo bar". */
export function describe(event: ActivityEvent): string {
  return phrases[event.action] ?? event.action.split(".").slice(1).join(" ").replace(/_/g, " ")
}

export function actorName(event: ActivityEvent, meId?: string) {
  if (event.actor.kind === "user" && event.actor.id === meId) return "You"
  if (event.actor.kind === "system") return "Paperboat"
  return event.actor.name ?? event.actor.email ?? (event.actor.kind === "machine" ? "A machine" : "Someone")
}

/** Where a target opens in the console, when it has a page. */
export function targetHref(event: ActivityEvent): string | undefined {
  const target = event.target
  if (!target) return
  if (target.kind === "machine") return `/machines/${encodeURIComponent(target.id)}`
  if (target.kind === "tunnel") return `/tunnels/${encodeURIComponent(target.id)}`
  if (target.kind === "terminal_session") return `/terminals/${encodeURIComponent(target.id)}`
}
