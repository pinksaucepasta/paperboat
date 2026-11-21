"use client"

import { infiniteQueryOptions, keepPreviousData, queryOptions, type QueryClient } from "@tanstack/react-query"
import { api, apiAction, ApiError, idempotencyKey, type Data, type Schemas, type Workspace } from "@/lib/api"
import { cursorPage, cursorPageSize } from "@/lib/cursor-pages"
import { pauseTunnelById, stopPreview } from "@/lib/tunnels"

/*
 * Self-hosted relays and tunnel servers (`/v1/selfhost/...`), each in a region the owner names (servers of one region back each other up). A node is installed on a Linux
 * server (`curl -fsSL <installer>/selfhost | sh`), prints a claim address and a
 * one-time code, and is paired here to your personal workspace or a team. A node running both
 * components appears as two installations with the same node ID. Pools say which nodes to use:
 * relays mix with Paperboat's or replace them (account-only); tunnel servers are either Paperboat's
 * or only yours, per workspace.
 */

export type Capability = "relay" | "tunnel"

export type Installation = Schemas["SelfhostInstallation"]

/** One of your own regions: a name you gave it and the slug machines carry (`id`, never changes). */
export type Region = Schemas["SelfhostRegion"]

/** selfhost.Policy: relay `mixed | self-hosted-only`, tunnel `managed-only | self-hosted-only`. */
export type Pool = Schemas["SelfhostPolicy"]

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
  regions: (scope: string) => ["selfhost", "regions", scope] as const,
  pool: (capability: Capability, scope: string) => ["selfhost", "pool", capability, scope] as const,
}

type InstallationsWire = Data<"/v1/selfhost/installations">

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

/** Your own regions (every region of the scope, empty ones too; Paperboat's regions never appear). */
export const regionsQuery = (scope: ReturnType<typeof scopeOf>) =>
  queryOptions({
    queryKey: networkKeys.regions(`${scope.kind}:${scope.id ?? ""}`),
    queryFn: ({ signal }) => api<Region[]>(`selfhost/regions?${scopeQuery(scope)}`, { signal }),
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
 * certificate), then assigns it to the scope. `expectRegion` and `expectCapability` make the server refuse a
 * machine installed for another region or role (400 `invalid_request`, naming what to fix). Answers with the
 * node's installations.
 */
export async function pairNode(input: {
  address: string
  code: string
  scope: ReturnType<typeof scopeOf>
  expectRegion?: string
  expectCapability?: Capability
}) {
  const result = await write<{ installations?: Installation[] } | Installation[]>("selfhost/enrollments", "POST", {
    address: input.address,
    code: input.code,
    scope: { kind: input.scope.kind, ...(input.scope.id && { id: input.scope.id }) },
    ...(input.expectRegion && { expect_region: input.expectRegion }),
    ...(input.expectCapability && { expect_capability: input.expectCapability }),
  })
  return Array.isArray(result) ? result : (result.installations ?? [])
}

/** Creates a region; the server derives its slug from the name (409 `region_exists` names the existing one). */
export const createRegion = (scope: ReturnType<typeof scopeOf>, name: string) =>
  write<Region>(`selfhost/regions?${scopeQuery(scope)}`, "POST", { name })

/** Renames only the display name; the slug, machines, pairs and pools are untouched. */
export const renameRegion = (scope: ReturnType<typeof scopeOf>, region: Region, name: string) =>
  write<Region>(`selfhost/regions/${encodeURIComponent(region.id)}?${scopeQuery(scope)}`, "PATCH", { name })

/** Removes an empty region (409 `region_in_use` says what to do first). */
export const deleteRegion = (scope: ReturnType<typeof scopeOf>, region: Region) =>
  apiAction(`selfhost/regions/${encodeURIComponent(region.id)}?${scopeQuery(scope)}`, { method: "DELETE" })

/**
 * After a machine is paired or removed, a region changes, or the relay pool changes: your regions, machines and pools,
 * and the Relays and Edges tables (Paperboat's relay regions, the workspace's pair of regions) that follow from them.
 */
export const refreshNetwork = (queryClient: QueryClient) =>
  Promise.all([["selfhost"], ["relays"], ["edges"]].map((queryKey) => queryClient.invalidateQueries({ queryKey })))

export const removeInstallation = (installation: Installation) =>
  write(`selfhost/installations/${encodeURIComponent(installation.installation_id)}`, "DELETE")

/** Asks the node to update to the newest self-host release at its next check-in (within a minute). */
export const requestUpdate = (installation: Installation) =>
  write(`selfhost/installations/${encodeURIComponent(installation.installation_id)}/update`, "POST", {})

/** An update the node is still running (the server records each step it reports). */
export const updating = (installation: Installation) =>
  Boolean(installation.update && !["succeeded", "rolled_back", "failed"].includes(installation.update.state))

/**
 * Drain: the machine takes no new work and running work finishes (up to 10 minutes); machines of the same
 * region keep serving. `move` also moves running work off now, and the machine drains within about 90 seconds.
 * Resume takes it back into service.
 */
export const drainInstallation = (installation: Installation, move: boolean) =>
  write(`selfhost/installations/${encodeURIComponent(installation.installation_id)}/drain`, "POST", { move })

export const resumeInstallation = (installation: Installation) =>
  write(`selfhost/installations/${encodeURIComponent(installation.installation_id)}/resume`, "POST", {})

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

/** One machine (a node), with the installations it runs (relay, edge, or both). */
export type Node = {
  id: string
  name: string
  host: string
  /** The region's slug. */
  region: string
  /** Some installation of the machine is drained (explicit on the installation). */
  draining: boolean
  installations: Installation[]
}

export function groupNodes(installations: Installation[]): Node[] {
  const nodes = new Map<string, Node>()
  for (const installation of installations) {
    const node = nodes.get(installation.node_id) ?? {
      id: installation.node_id,
      name: installation.name,
      host: installation.endpoint_host,
      region: installation.region,
      draining: false,
      installations: [],
    }
    node.installations.push(installation)
    node.draining ||= installation.draining
    nodes.set(installation.node_id, node)
  }
  return [...nodes.values()].sort((a, b) => a.region.localeCompare(b.region) || a.name.localeCompare(b.name))
}

/** The machine's roles as the console names them: a tunnel server is an Edge. */
export const roleLabel = (capability: Capability) => (capability === "tunnel" ? "Edge" : "Relay")

/** What a region needs, in order: its machines are all ready, some are, none are, it is being drained, or it has none. */
export type RegionState = "ready" | "degraded" | "draining" | "not_ready" | "empty"

export function regionState(region: Region, nodes: Node[]): RegionState {
  if (region.machines === 0 && nodes.length === 0) return "empty"
  if (nodes.length > 0 && nodes.every((node) => node.draining)) return "draining"
  if (region.machines === 0) return "not_ready"
  if (region.ready >= region.machines) return "ready"
  return region.ready === 0 ? "not_ready" : "degraded"
}

/** The installer for a machine, run on a Linux server: it carries the role and the region the machine joins. */
export const installCommand = (installerBaseUrl: string, role: "relay" | "edge", regionSlug: string) =>
  `curl -fsSL ${installerBaseUrl}/selfhost | sh -s -- --${role} --region ${regionSlug}`

/** One of a workspace's two regions: one of Paperboat's, or one of your own (the region you named when pairing servers). */
export type PairRegion = Data<"/v1/edges/pair">["primary"]
export type EdgePair = Data<"/v1/edges/pair">
export type PairKind = EdgePair["kind"]

/**
 * The workspace's pair of regions (where its tunnels and previews are served); null while none is
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

/** What to save: both regions are Paperboat's (`managed`) or both are your own (`self_hosted`). */
export type PairChoice = { kind: PairKind; primaryRegion: string; fallbackRegion: string }

export const saveEdgePair = (workspace: string, choice: PairChoice) =>
  write<EdgePair>(`edges/pair?${new URLSearchParams({ workspace })}`, "PUT", {
    kind: choice.kind,
    primary_region: choice.primaryRegion,
    fallback_region: choice.fallbackRegion,
  })

/** A preview or tunnel still running on the current edges: what blocks a switch (409 previews_running). */
export type LiveItem = { kind: "preview" | "tunnel"; id: string; name: string }

/** What the server refused a switch for, from its 409 `previews_running` error; undefined for any other error. */
export function liveOf(error: unknown): { count: number; items: LiveItem[] } | undefined {
  if (!(error instanceof ApiError) || error.code !== "previews_running") return undefined
  const details = (error.details ?? {}) as { count?: number; items?: LiveItem[] }
  const items = Array.isArray(details.items) ? details.items : []
  return { count: Number(details.count ?? items.length), items }
}

/** Ends what is running: previews stop, tunnels pause (they keep their routes). Every item is tried; the first failure is thrown after. */
async function stopLive(items: LiveItem[]) {
  const results = await Promise.allSettled(
    items.map((item) => (item.kind === "preview" ? stopPreview(item.id) : pauseTunnelById(item.id))),
  )
  const failed = results.find((result) => result.status === "rejected")
  if (failed) throw failed.reason
}

/**
 * Switches the pair, stopping whatever the server names as running first and asking again. The server names at
 * most 50 at a time, so a larger workspace takes a few rounds; the stopped items come back so the dialog can
 * say what to start again.
 */
export async function switchEdgesStoppingLive(workspace: string, choice: PairChoice) {
  const stopped: LiveItem[] = []
  for (let round = 0; ; round++) {
    try {
      return { pair: await saveEdgePair(workspace, choice), stopped }
    } catch (error) {
      const live = liveOf(error)
      if (!live || live.items.length === 0 || round >= 10) throw error
      // shortcut: items stopped before a failed round are not remembered for the cutover text, only what a later round stops.
      await stopLive(live.items)
      stopped.push(...live.items)
    }
  }
}
