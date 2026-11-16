"use client"

import { queryOptions } from "@tanstack/react-query"
import { api, idempotencyKey, type Data, type Schemas } from "@/lib/api"

/*
 * Team SSO and directory sync (F19, server/docs/teams-business.md). The shape is OpenAPI's
 * TeamIdentityStatus as the server sends it (teamidentity.Status): every list is present, possibly
 * empty; only `directory.directory` can be null. Owners and admins change it (`security.manage`);
 * auditors see it.
 */

/** `type` is WorkOS's connection type, e.g. OktaSAML, AzureSAML, GoogleOAuth, GenericOIDC. */
export type TeamIdentity = Schemas["TeamIdentityStatus"]
export type SsoConnection = TeamIdentity["sso"]["connections"][number]
export type SsoDomain = TeamIdentity["sso"]["domains"][number]
/** `role` "" means the group gives no role. */
export type DirectoryGroup = TeamIdentity["directory"]["groups"][number]

/** What the WorkOS Admin Portal opens for: verify a domain, set up SSO, connect a directory. */
export type PortalIntent = "domain_verification" | "sso" | "dsync"

const path = (team: string) => `teams/${encodeURIComponent(team)}/identity`
const json = { "content-type": "application/json" }

export const identityQuery = (team: string) =>
  queryOptions({
    queryKey: ["team", team, "identity"],
    queryFn: ({ signal }) => api<TeamIdentity>(path(team), { signal }),
    staleTime: 30_000,
  })

/** A one-time Admin Portal link; it returns to /team?team=<id>&tab=security. */
export const openPortal = (team: string, intent: PortalIntent) =>
  api<Data<"/v1/teams/{team_id}/identity/portal", "post">>(`${path(team)}/portal`, { method: "POST", headers: json, body: JSON.stringify({ intent }) })

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
