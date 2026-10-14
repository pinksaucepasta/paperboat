"use client"

import { infiniteQueryOptions } from "@tanstack/react-query"
import { api } from "@/lib/api"

/*
 * Activity: what happened in the workspace, newest first. The server writes audit events
 * (`audit.Write`) but has no read API yet; this is the console's side of the planned
 * `GET /v1/activity` (NOTES.md).
 */

export type ActivityEvent = {
  id: string
  occurred_at: string
  /** `area.verb`, e.g. `terminal.opened`, `tunnel.created`, `machine.removed`. */
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
  publish: "preview,tunnel",
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

/** What happened, as the middle of a sentence: "opened a terminal", "removed the tunnel". */
const phrases: Record<string, string> = {
  "machine.added": "added the machine",
  "machine.removed": "removed the machine",
  "machine.renamed": "renamed the machine",
  "machine.updated": "updated Paperboat on",
  "machine.shared": "shared the machine",
  "terminal.opened": "opened a terminal",
  "terminal.closed": "closed the terminal",
  "terminal.shared": "shared the terminal",
  "terminal.joined": "joined the terminal",
  "preview.created": "started the preview",
  "preview.stopped": "stopped the preview",
  "tunnel.created": "created the tunnel",
  "tunnel.deleted": "deleted the tunnel",
  "tunnel.paused": "paused the tunnel",
  "tunnel.resumed": "resumed the tunnel",
  "tunnel.hostname_added": "added a public hostname to",
  "tunnel.hostname_removed": "removed a public hostname from",
  "file.sent": "sent",
  "file.deleted": "deleted",
  "file.shared": "shared a link to",
  "environment.changed": "changed environment variables",
  "config.conflict": "found a config conflict",
  "team.member_added": "added a member:",
  "team.member_removed": "removed a member:",
  "team.invited": "invited",
  "share.granted": "gave access to",
  "share.revoked": "removed access to",
  "account.signed_in": "signed in",
}

export function describe(event: ActivityEvent): string {
  return phrases[event.action] ?? event.action.replace(/[._]/g, " ")
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
