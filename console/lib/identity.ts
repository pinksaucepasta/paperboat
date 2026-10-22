"use client"

import { queryOptions } from "@tanstack/react-query"
import { api, idempotencyKey } from "@/lib/api"

/*
 * Team SSO and directory sync (F19, server/docs/teams-business.md). The shapes follow Lane S's
 * contract of 2026-10-23 until server/docs/openapi.json carries them; then they come from
 * api-schema.d.ts. Owners and admins change them (`security.manage`); auditors see them.
 */

export type SsoConnection = { id: string; name?: string; type?: string; state?: string }
export type SsoDomain = { id?: string; domain: string; state?: string }
export type DirectoryGroup = { id: string; name: string; role: string }

export type TeamIdentity = {
  eligible: boolean
  min_seats: number
  seats: number
  organization: string | null
  can_manage: boolean
  sso: {
    connections: SsoConnection[]
    domains: SsoDomain[]
    required: boolean
    session_seconds: number
    session_min_seconds: number
    session_max_seconds: number
    jit: boolean
    /** Account IDs that may sign in without SSO while it is required. */
    exempt: string[]
    exempt_max: number
  }
  directory: {
    directory: { id: string; name?: string; type?: string; state?: string } | null
    users: number
    groups: DirectoryGroup[]
    mapped_only: boolean
    synced_at?: string
    error?: string
  }
}

/** What the WorkOS Admin Portal opens for: verify a domain, set up SSO, connect a directory. */
export type PortalIntent = "domain_verification" | "sso" | "dsync"

const path = (team: string) => `teams/${encodeURIComponent(team)}/identity`
const json = { "content-type": "application/json" }

export const identityQuery = (team: string) =>
  queryOptions({
    queryKey: ["team", team, "identity"],
    queryFn: () => api<TeamIdentity>(path(team)),
    staleTime: 30_000,
  })

/** A one-time Admin Portal link; it returns to /team?team=<id>&tab=security. */
export const openPortal = (team: string, intent: PortalIntent) =>
  api<{ url: string }>(`${path(team)}/portal`, { method: "POST", headers: json, body: JSON.stringify({ intent }) })

export type SsoChange = { required?: boolean; confirm?: string; session_seconds?: number; jit?: boolean }

export const updateSso = (team: string, change: SsoChange) =>
  api<TeamIdentity>(`${path(team)}/sso`, {
    method: "PUT",
    headers: { ...json, "idempotency-key": idempotencyKey() },
    body: JSON.stringify(change),
  })

export const setExempt = (team: string, accountId: string, exempt: boolean) =>
  api<TeamIdentity>(`${path(team)}/exempt`, {
    method: "PUT",
    headers: { ...json, "idempotency-key": idempotencyKey() },
    body: JSON.stringify({ account_id: accountId, exempt }),
  })

export const updateDirectory = (team: string, groupRoles: Record<string, string>, mappedOnly: boolean) =>
  api<TeamIdentity>(`${path(team)}/directory`, {
    method: "PUT",
    headers: { ...json, "idempotency-key": idempotencyKey() },
    body: JSON.stringify({ group_roles: groupRoles, mapped_only: mappedOnly }),
  })

/** Members who would lose access when SSO becomes required (409 sso_review_required details). */
export type SsoReview = { members: { account_id: string; email: string }[]; digest: string }
