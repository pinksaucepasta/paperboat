"use client"

import { queryOptions } from "@tanstack/react-query"
import { api, idempotencyKey, useApiQuery, useWorkspace } from "@/lib/api"

/*
 * Your domains, added once per workspace and verified with one TXT record. A verified domain can
 * then be used for machine addresses (`homelab.home.acme.dev`), tunnel public hostnames
 * (`app.acme.dev`, `*.acme.dev`) and preview addresses (`demo.acme.dev`). This is the console's
 * side of a planned account-level API (NOTES.md); domains used on tunnels and previews today are
 * bound per resource.
 */

export type AccountDomain = {
  id: string
  /** The domain as added, e.g. `acme.dev`. */
  name: string
  state: "pending" | "verified" | "failed"
  /** The TXT record that proves you own it, shown until it's verified. */
  verification?: { type: "TXT"; name: string; value: string }
  created_at: string
}

const workspaceQuery = (workspace: string) => `workspace=${encodeURIComponent(workspace)}`

export const accountDomainsQuery = (workspace: string) =>
  queryOptions({
    queryKey: ["domains", workspace] as const,
    queryFn: async () => (await api<{ items: AccountDomain[] }>(`domains?${workspaceQuery(workspace)}`)).items,
    retry: 1,
  })

/** Names of your verified domains, for the domain pickers in tunnels and previews. */
export function useVerifiedDomains(): string[] {
  const workspace = useWorkspace()
  const domains = useApiQuery({ ...accountDomainsQuery(workspace?.id ?? ""), enabled: Boolean(workspace) })
  return (domains.data ?? []).filter((domain) => domain.state === "verified").map((domain) => domain.name)
}

const write = <T>(path: string, method: string, body?: unknown) =>
  api<T>(path, {
    method,
    headers: { "idempotency-key": idempotencyKey(), "content-type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  })

export const addAccountDomain = (workspace: string, name: string) =>
  write<AccountDomain>(`domains?${workspaceQuery(workspace)}`, "POST", { name })
export const verifyAccountDomain = (domain: AccountDomain) =>
  write<AccountDomain>(`domains/${encodeURIComponent(domain.id)}/verify`, "POST", {})
export const removeAccountDomain = (domain: AccountDomain) =>
  write(`domains/${encodeURIComponent(domain.id)}`, "DELETE")

/** Whether `hostname` (or a wildcard of it) is `domain` or under it. */
export const isUnder = (hostname: string, domain: string) => {
  const host = hostname.replace(/^\*\./, "")
  return host === domain || host.endsWith(`.${domain}`)
}

/** A domain as people type it: lowercase, no scheme, no trailing dot. */
export const cleanDomain = (value: string) =>
  value
    .trim()
    .toLowerCase()
    .replace(/^[a-z]+:\/\//, "")
    .replace(/\/.*$/, "")
    .replace(/^\.+|\.+$/g, "")

export const domainPattern = /^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+[a-z]{2,}$/
