"use client"

import { queryOptions } from "@tanstack/react-query"
import { api, apiAction, type Data } from "@/lib/api"

/*
 * Domains added to the workspace (GET/POST /v1/domains). A domain is verified once by its
 * _acme-challenge CNAME; then any name one level under it can be used by tunnels and previews,
 * with HTTPS from the domain's one wildcard certificate.
 */

export type WorkspaceDomain = Data<"/v1/domains">["items"][number]

const path = (workspace: string, id?: string, suffix = "") =>
  `domains${id ? `/${encodeURIComponent(id)}${suffix}` : ""}?workspace=${encodeURIComponent(workspace)}`

const settling = (domain: WorkspaceDomain) => domain.state === "pending_dns" || domain.certificate.state === "pending"

export const workspaceDomainsQuery = (workspace: string) =>
  queryOptions({
    queryKey: ["workspace-domains", workspace],
    queryFn: async ({ signal }) => (await api<{ items: WorkspaceDomain[] }>(path(workspace), { signal })).items,
    // Fast while DNS or the certificate is on its way, slow once everything is settled.
    refetchInterval: (query) => (query.state.data?.some(settling) ? 5_000 : 60_000),
  })

export const addWorkspaceDomain = (workspace: string, hostname: string) =>
  api<WorkspaceDomain>(path(workspace), {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ hostname }),
  })

export const checkWorkspaceDomain = (workspace: string, id: string) =>
  api<WorkspaceDomain>(path(workspace, id, "/check"), { method: "POST" })

export const removeWorkspaceDomain = (workspace: string, id: string) =>
  apiAction(path(workspace, id), { method: "DELETE" })

export type Tone = "ok" | "wait" | "bad"

/** A domain's state in a few words: DNS first, then the certificate. */
export function domainStatus(domain: WorkspaceDomain): { tone: Tone; label: string } {
  if (domain.state === "failed") return { tone: "bad", label: "DNS record not found" }
  if (domain.state === "pending_dns")
    return domain.dns_failure?.code === "wrong_record"
      ? { tone: "bad", label: "DNS record points elsewhere" }
      : { tone: "wait", label: "Waiting for DNS" }
  if (domain.certificate.state === "failed") return { tone: "bad", label: "Certificate failed" }
  if (domain.certificate.state === "pending") return { tone: "wait", label: "Getting a certificate" }
  return { tone: "ok", label: "Ready" }
}

/** The domains a tunnel or preview hostname can use: verified ones. */
export const usableDomains = (domains: WorkspaceDomain[] | undefined) =>
  (domains ?? []).filter((domain) => domain.state === "verified").map((domain) => domain.hostname)
