"use client"

import { queryOptions } from "@tanstack/react-query"
import { api, idempotencyKey, type Workspace } from "@/lib/api"

/*
 * Self-hosted relays and tunnel servers (`/v1/selfhost/...`). A node is installed on a Linux
 * server (`curl -fsSL https://get.pprbt.dev/selfhost/install | sh`), prints a claim address and a
 * one-time code, and is paired here to your personal workspace or a team. A node running both
 * components appears as two installations with the same node ID. Pools say which nodes to use:
 * relays mix with Paperboat's or replace them (account-only); tunnel servers are either Paperboat's
 * or only yours, per workspace.
 */

export type Capability = "relay" | "tunnel"

/** selfhost.Installation */
export type Installation = {
  installation_id: string
  node_id: string
  endpoint_host: string
  name: string
  capability: Capability
  ready: boolean
  scope_kind: "account" | "team" | "global"
  scope_id: string
  trust_notice: string
  enrollment_state: "pending" | "enrolled"
}

/** selfhost.Policy: relay `mixed | self-hosted-only`, tunnel `managed-only | self-hosted-only`. */
export type Pool = { mode: "mixed" | "managed-only" | "self-hosted-only"; installation_ids: string[] }

/** The scope a workspace's nodes and tunnel pool belong to. */
export function scopeOf(workspace: Workspace | undefined) {
  return workspace?.kind === "team"
    ? { kind: "team" as const, id: workspace.id }
    : { kind: "account" as const, id: undefined }
}

const scopeQuery = (scope: ReturnType<typeof scopeOf>) =>
  new URLSearchParams({ scope_kind: scope.kind, ...(scope.id && { scope_id: scope.id }) })

export const networkKeys = {
  installations: (scope: string) => ["selfhost", "installations", scope] as const,
  pool: (capability: Capability, scope: string) => ["selfhost", "pool", capability, scope] as const,
}

export const installationsQuery = (scope: ReturnType<typeof scopeOf>) =>
  queryOptions({
    queryKey: networkKeys.installations(`${scope.kind}:${scope.id ?? ""}`),
    queryFn: async () =>
      (await api<{ installations: Installation[] }>(`selfhost/installations?${scopeQuery(scope)}&limit=200`))
        .installations,
    // Readiness follows the node's own registration and health reports.
    refetchInterval: 10_000,
  })

/** Relay pools are account-only; tunnel pools follow the workspace. */
export const poolQuery = (capability: Capability, scope: ReturnType<typeof scopeOf>) => {
  const effective = capability === "relay" ? scopeOf(undefined) : scope
  return queryOptions({
    queryKey: networkKeys.pool(capability, `${effective.kind}:${effective.id ?? ""}`),
    queryFn: () => api<Pool>(`selfhost/pools/${capability}?${scopeQuery(effective)}`),
  })
}

const write = <T>(path: string, method: string, body?: unknown) =>
  api<T>(path, {
    method,
    headers: { "idempotency-key": idempotencyKey(), "content-type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  })

/**
 * Pairs a node: the server checks the code against the node at `address` (the code pins its
 * certificate), then assigns it to the scope. Answers with the node's installations.
 */
export async function pairNode(input: { address: string; code: string; scope: ReturnType<typeof scopeOf> }) {
  const result = await write<{ installations?: Installation[] } | Installation[]>("selfhost/enrollments", "POST", {
    address: input.address,
    code: input.code,
    scope: { kind: input.scope.kind, ...(input.scope.id && { id: input.scope.id }) },
  })
  return Array.isArray(result) ? result : (result.installations ?? [])
}

export const removeInstallation = (installation: Installation) =>
  write(`selfhost/installations/${encodeURIComponent(installation.installation_id)}`, "DELETE")

/** Tunnel servers: checks the node's public ingress answers before it's used for publications. */
export const verifyIngress = (installation: Installation) =>
  write(`selfhost/installations/${encodeURIComponent(installation.installation_id)}/verify-ingress`, "POST", {})

export const savePool = (capability: Capability, scope: ReturnType<typeof scopeOf>, pool: Pool) => {
  const effective = capability === "relay" ? scopeOf(undefined) : scope
  return write<Pool>(`selfhost/pools/${capability}?${scopeQuery(effective)}`, "PUT", pool)
}

/** One server, with the installations it runs (relay, tunnel server, or both). */
export type Node = { id: string; name: string; host: string; installations: Installation[] }

export function groupNodes(installations: Installation[]): Node[] {
  const nodes = new Map<string, Node>()
  for (const installation of installations) {
    const node = nodes.get(installation.node_id) ?? {
      id: installation.node_id,
      name: installation.name,
      host: installation.endpoint_host,
      installations: [],
    }
    node.installations.push(installation)
    nodes.set(installation.node_id, node)
  }
  return [...nodes.values()].sort((a, b) => a.name.localeCompare(b.name))
}

/** The installer for a node, run on the server. */
export const installCommand = (components: "relay" | "tunnel" | "both") =>
  `curl -fsSL https://get.pprbt.dev/selfhost/install | sh${components === "relay" ? "" : ` -s -- --enable ${components}`}`
