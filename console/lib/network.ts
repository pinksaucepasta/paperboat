"use client"

import { infiniteQueryOptions, keepPreviousData, queryOptions } from "@tanstack/react-query"
import { api, ApiError, idempotencyKey, type Data, type Workspace } from "@/lib/api"
import { cursorPage, cursorPageSize } from "@/lib/cursor-pages"

/*
 * Self-hosted relays and tunnel servers (`/v1/selfhost/...`). A node is installed on a Linux
 * server (`curl -fsSL <installer>/selfhost/install | sh`), prints a claim address and a
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
  last_heartbeat_at?: string
}

/** selfhost.Policy: relay `mixed | self-hosted-only`, tunnel `managed-only | self-hosted-only`. */
export type Pool = {
  mode: "mixed" | "managed-only" | "self-hosted-only"
  installation_ids: string[]
  /** The saved pool's servers, named (read only; not sent back when saving). */
  members?: { installation_id: string; name: string; endpoint_host: string }[]
}

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

type InstallationsWire = { installations: Installation[] | null; pagination: { total: number; next_cursor?: string } }

const readInstallations = (raw: InstallationsWire) => ({
  items: raw.installations,
  total: raw.pagination.total,
  next_cursor: raw.pagination.next_cursor,
})

/** One page of the scope's installations, searched and paged by the server. */
export const installationsQuery = (scope: ReturnType<typeof scopeOf>, q: string, page: number) =>
  queryOptions({
    queryKey: [...networkKeys.installations(`${scope.kind}:${scope.id ?? ""}`), q, page],
    queryFn: ({ signal }) =>
      cursorPage<Installation, InstallationsWire>(
        "selfhost/installations",
        { ...Object.fromEntries(scopeQuery(scope)), ...(q && { q }) },
        page,
        signal,
        readInstallations,
      ),
    placeholderData: keepPreviousData,
    // Readiness follows the node's own registration and health reports.
    refetchInterval: 10_000,
  })

/** The enrolled servers a pool may hold, searched on the server and read page by page (load more). */
export const poolServersQuery = (capability: Capability, scope: ReturnType<typeof scopeOf>, q: string) =>
  infiniteQueryOptions({
    queryKey: [...networkKeys.installations(`${scope.kind}:${scope.id ?? ""}`), "pool-servers", capability, q],
    queryFn: async ({ pageParam, signal }) => {
      const query = new URLSearchParams({ limit: String(cursorPageSize), capability, enrollment_state: "enrolled" })
      for (const [key, value] of scopeQuery(scope)) query.set(key, value)
      if (q) query.set("q", q)
      if (pageParam) query.set("cursor", pageParam)
      const page = await api<InstallationsWire>(`selfhost/installations?${query}`, { signal })
      return { items: page.installations ?? [], total: page.pagination.total, next_cursor: page.pagination.next_cursor }
    },
    initialPageParam: "",
    getNextPageParam: (page) => page.next_cursor || undefined,
    refetchInterval: 10_000,
  })

export const poolQuery = (capability: Capability, scope: ReturnType<typeof scopeOf>) => {
  const effective = capability === "relay" ? scopeOf(undefined) : scope
  return queryOptions({
    queryKey: networkKeys.pool(capability, `${effective.kind}:${effective.id ?? ""}`),
    queryFn: ({ signal }) => api<Pool>(`selfhost/pools/${capability}?${scopeQuery(effective)}`, { signal }),
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
  return write<Pool>(`selfhost/pools/${capability}?${scopeQuery(effective)}`, "PUT", {
    mode: pool.mode,
    installation_ids: pool.installation_ids,
  })
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
export const installCommand = (installerBaseUrl: string, components: "relay" | "tunnel" | "both") =>
  `curl -fsSL ${installerBaseUrl}/selfhost/install | sh${components === "relay" ? "" : ` -s -- --enable ${components}`}`

/** One of a workspace's two edges: a Paperboat edge or one of your tunnel servers. */
export type PairEdge = Data<"/v1/edges/pair">["primary"]
export type EdgePair = Data<"/v1/edges/pair">

/**
 * The workspace's edge pair (where its tunnels and previews are served); null while none is
 * assigned (404 edge_pair_unassigned). Machines pick between the two by what they measure.
 */
export const edgePairQuery = (workspace: string) =>
  queryOptions({
    queryKey: ["edges", "pair", workspace],
    queryFn: async ({ signal }) => {
      try {
        return await api<EdgePair>(`edges/pair?${new URLSearchParams({ workspace })}`, { signal })
      } catch (error) {
        if (error instanceof ApiError && error.code === "edge_pair_unassigned") return null
        throw error
      }
    },
    refetchInterval: 30_000,
  })

/** Sets the pair; both ids are Paperboat edges or both your tunnel servers (installation ids). */
export const saveEdgePair = (workspace: string, primaryId: string, fallbackId: string) =>
  write<EdgePair>(`edges/pair?${new URLSearchParams({ workspace })}`, "PUT", {
    primary_id: primaryId,
    fallback_id: fallbackId,
  })
