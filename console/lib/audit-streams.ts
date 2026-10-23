"use client"

import { queryOptions } from "@tanstack/react-query"
import { api, idempotencyKey } from "@/lib/api"
import type { components } from "@/lib/api-schema"

/** Team audit log streaming to a SIEM (F19, teams-business.md "Streaming to a SIEM"). */
export type AuditStream = components["schemas"]["AuditStreamDestination"]
export type AuditStreamInput = components["schemas"]["AuditStreamInput"]
export type StreamKind = AuditStream["kind"]

const path = (team: string) => `teams/${encodeURIComponent(team)}/audit-streams`

export const auditStreamsQuery = (team: string) =>
  queryOptions({
    queryKey: ["team", team, "audit-streams"],
    queryFn: () => api<AuditStream[]>(path(team)),
    staleTime: 15_000,
  })

export const saveAuditStream = (team: string, kind: StreamKind, input: AuditStreamInput) =>
  api<AuditStream>(`${path(team)}/${kind}`, {
    method: "PUT",
    headers: { "content-type": "application/json", "idempotency-key": idempotencyKey() },
    body: JSON.stringify(input),
  })

export const removeAuditStream = (team: string, kind: StreamKind) =>
  api<unknown>(`${path(team)}/${kind}`, { method: "DELETE", headers: { "idempotency-key": idempotencyKey() } })

/** Delivers one team.activity_stream_test event now; 502 audit_stream_delivery_failed says why not. */
export const testAuditStream = (team: string, kind: StreamKind) =>
  api<unknown>(`${path(team)}/${kind}/test`, { method: "POST", headers: { "idempotency-key": idempotencyKey() } })
