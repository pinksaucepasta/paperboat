"use client"

import { queryOptions } from "@tanstack/react-query"
import { api, idempotencyKey } from "@/lib/api"

/*
 * Team SSO and directory sync (F19, server/docs/teams-business.md). The shape is OpenAPI's
 * TeamIdentityStatus as the server sends it (teamidentity.Status): every list is present, possibly
 * empty; only `directory.directory` can be null. Owners and admins change it (`security.manage`);
 * auditors see it.
 */

/** `type` is WorkOS's connection type, e.g. OktaSAML, AzureSAML, GoogleOAuth, GenericOIDC. */
export type SsoConnection = { id: string; type: string; state: "active" | "inactive" | "validating" }
export type SsoDomain = { domain: string; state: "verified" | "pending" | "failed" }
/** `role` "" means the group gives no role. */
export type DirectoryGroup = { id: string; name: string; role: string }

export type TeamIdentity = {
  eligible: boolean
  min_seats: number
  seats: number
  /** Whether the team has an identity organization yet (created on first Admin Portal use). */
  organization: boolean
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
    directory: { id: string; type: string; state: string } | null
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

/** WorkOS type names as people know them: "OktaSAML" → "Okta SAML", "GenericOIDC" → "OIDC". */
export function providerName(type: string): string {
  const known: Record<string, string> = {
    GenericSAML: "SAML",
    GenericOIDC: "OIDC",
    GoogleOAuth: "Google",
    MicrosoftOAuth: "Microsoft",
    AzureSAML: "Microsoft Entra ID (SAML)",
    AzureSCIMV2_0: "Microsoft Entra ID",
    OktaSCIMV2_0: "Okta",
    GenericSCIMV2_0: "SCIM",
    GoogleWorkspace: "Google Workspace",
  }
  // Vendor names keep their own spelling (OneLogin, JumpCloud); only the protocol is set apart.
  return known[type] ?? type.replace(/V\d_\d$/, "").replace(/(SAML|OIDC|SCIM|OAuth)$/, " $1").trim()
}
