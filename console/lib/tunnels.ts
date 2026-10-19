"use client"

import { queryOptions } from "@tanstack/react-query"
import { api, ApiError, idempotencyKey, type Data } from "@/lib/api"
import { uploadFile } from "@/lib/upload"

/*
 * The server's tunnel API. Previews and tunnels are one tunnel resource with two lifecycles (plans: "Preview and tunnel
 * strategy"): a preview is a temporary lease on a generated address, a tunnel is durable
 * desired state with routes and domains. Both are created on the machine itself (the server
 * accepts creation only with the machine's signed proof), so the console lists, inspects,
 * changes and stops them, and hands out the `pb` command for creating one.
 */

export type Preview = Data<"/v1/previews">["items"][number]
export type Tunnel = Data<"/v1/tunnels">["items"][number]
export type TunnelRoute = Data<"/v1/tunnels/{tunnel_id}/routes">["items"][number]
export type TunnelDomain = Data<"/v1/tunnels/{tunnel_id}/domains">["items"][number]
export type TunnelHealth = Data<"/v1/tunnels/{tunnel_id}/status">
export type DnsInstructions = Data<"/v1/tunnels/{tunnel_id}/domains/{domain_id}/instructions">

const previewPath = (id: string) => `previews/${encodeURIComponent(id)}`
const tunnelPath = (id: string) => `tunnels/${encodeURIComponent(id)}`

export const tunnelKeys = {
  previews: (workspace: string) => ["previews", workspace] as const,
  tunnels: (workspace: string) => ["tunnels", workspace] as const,
  tunnel: (id: string) => ["tunnel", id] as const,
  health: (id: string) => ["tunnel", id, "health"] as const,
  routes: (id: string) => ["tunnel", id, "routes"] as const,
  domains: (id: string) => ["tunnel", id, "domains"] as const,
  instructions: (id: string, domainId: string) => ["tunnel", id, "domains", domainId, "instructions"] as const,
}

/** Every page of a cursor collection, bounded so one huge account cannot stall the page. */
async function allPages<T>(path: string, params: Record<string, string> = {}): Promise<T[]> {
  const items: T[] = []
  let cursor = ""
  for (let page = 0; page < 10; page++) {
    const query = new URLSearchParams({ ...params, limit: "200", ...(cursor && { cursor }) })
    const result = await api<{ items: T[]; next_cursor?: string }>(`${path}?${query}`)
    items.push(...result.items)
    if (!result.next_cursor) break
    cursor = result.next_cursor
  }
  return items
}

/** The workspace's previews, live and ended. Leases count down, so the list stays fresh. */
export const previewsQuery = (workspace: string) =>
  queryOptions({
    queryKey: tunnelKeys.previews(workspace),
    queryFn: () => allPages<Preview>("previews", { workspace }),
    refetchInterval: 10_000,
  })

/** The workspace's tunnels. Deleted ones are tombstones the server keeps; they are not shown. */
export const tunnelsQuery = (workspace: string) =>
  queryOptions({
    queryKey: tunnelKeys.tunnels(workspace),
    queryFn: async () =>
      (await allPages<Tunnel>("tunnels", { workspace })).filter((tunnel) => tunnel.desired_state !== "deleted"),
    refetchInterval: 15_000,
  })

export const tunnelQuery = (id: string) =>
  queryOptions({
    queryKey: tunnelKeys.tunnel(id),
    queryFn: () => api<Tunnel>(tunnelPath(id)),
    refetchInterval: 10_000,
  })

export const tunnelHealthQuery = (id: string) =>
  queryOptions({
    queryKey: tunnelKeys.health(id),
    queryFn: () => api<TunnelHealth>(`${tunnelPath(id)}/status`),
    refetchInterval: 10_000,
  })

export const routesQuery = (id: string) =>
  queryOptions({
    queryKey: tunnelKeys.routes(id),
    queryFn: async () =>
      (await allPages<TunnelRoute>(`${tunnelPath(id)}/routes`)).filter((route) => route.desired_state !== "deleted"),
  })

export const domainsQuery = (id: string) =>
  queryOptions({
    queryKey: tunnelKeys.domains(id),
    queryFn: () => allPages<TunnelDomain>(`${tunnelPath(id)}/domains`),
    // Domains move through DNS and certificate states on their own.
    refetchInterval: 15_000,
  })

export const instructionsQuery = (id: string, domainId: string) =>
  queryOptions({
    queryKey: tunnelKeys.instructions(id, domainId),
    queryFn: () => api<DnsInstructions>(`${tunnelPath(id)}/domains/${encodeURIComponent(domainId)}/instructions`),
  })

/**
 * A change to a preview or tunnel resource. Every one carries an idempotency key, and changes to
 * an existing resource carry its ETag, so an edit made from an outdated page is refused (412)
 * instead of overwriting a newer one.
 */
function change<T = unknown>(path: string, method: string, options: { etag?: string; body?: unknown } = {}) {
  return api<T>(path, {
    method,
    headers: {
      "idempotency-key": idempotencyKey(),
      ...(options.etag && { "if-match": options.etag }),
      ...(options.body !== undefined && { "content-type": "application/json" }),
    },
    body: options.body === undefined ? undefined : JSON.stringify(options.body),
  })
}

/**
 * Stops a preview. The lease carries no ETag in its body, so the current one comes from reading
 * it first; a preview that changed in between is still the one being stopped.
 */
export async function stopPreview(id: string) {
  const response = await fetch(`/api/v1/${previewPath(id)}`, { headers: { accept: "application/json" } })
  const etag = response.headers.get("etag")
  if (!response.ok || !etag) {
    if (response.status === 404) return
    throw new ApiError(response.status, "unavailable", "Couldn’t read the preview to stop it. Try again.")
  }
  await change(previewPath(id), "DELETE", { etag })
}

export const previewQuery = (id: string) =>
  queryOptions({
    queryKey: ["preview", id] as const,
    queryFn: () => api<Preview>(previewPath(id)),
  })

/** A fresh owner session per create (3 to 128 characters, no whitespace); the machine binds it on first dispatch. */
function ownerSessionId() {
  return `console_${idempotencyKey()}`
}

/**
 * Starts a preview from the console. The server records the lease and hands it to the machine's
 * Paperboat service (a background lease, like `pb preview --background --ttl`), which connects
 * it; the answer is either the lease or the operation still connecting it. Returns its ID.
 */
export async function createPreview(input: {
  machineId: string
  workspace: string
  port: number
  access: Preview["access_mode"]
  hours: number
  /** An address on one of your domains, besides the generated one. */
  hostname?: string
}) {
  const result = await change<{ kind: string; id: string; resource_id?: string }>(
    `previews?workspace=${encodeURIComponent(input.workspace)}`,
    "POST",
    {
      body: {
        workspace: input.workspace,
        owner_machine_id: input.machineId,
        owner_session_id: ownerSessionId(),
        owner_session_kind: "foreground",
        target: { scheme: "http", address: `localhost:${input.port}` },
        access_mode: input.access,
        expires_at: new Date(Date.now() + input.hours * 3_600_000).toISOString(),
        domains: input.hostname ? [input.hostname] : [],
      },
    },
  )
  return result.kind === "operation" ? result.resource_id! : result.id
}

/** Files that open in the browser from a shared-file preview; anything else downloads. */
export function opensInBrowser(file: { name: string; type: string }) {
  return (
    /^(text\/|image\/|video\/|audio\/)/.test(file.type) ||
    file.type === "application/pdf" ||
    /\.(html?|md|markdown|txt|pdf|png|jpe?g|gif|webp|svg|json|csv)$/i.test(file.name)
  )
}

/**
 * Shares one file as a preview: the browser sends it to the chosen machine (the server passes it
 * through and keeps nothing), the machine serves it at a new preview address, and the call answers
 * with the preview's ID. HTML, Markdown and other viewable files open in the browser; the rest
 * download.
 */
export function shareFile(
  input: { file: File; machineId: string; workspace: string; access: Preview["access_mode"]; hours: number },
  onProgress: (fraction: number) => void,
): { done: Promise<string>; cancel: () => void } {
  const query = new URLSearchParams({
    workspace: input.workspace,
    machine: input.machineId,
    name: input.file.name,
    access_mode: input.access,
    expires_at: new Date(Date.now() + input.hours * 3_600_000).toISOString(),
  })
  const upload = uploadFile<OperationOrResource>(`previews/files?${query}`, input.file, onProgress)
  return { done: upload.done.then(resourceId), cancel: upload.cancel }
}

export const pauseTunnel = (tunnel: Tunnel) => change(`${tunnelPath(tunnel.id)}/pause`, "POST", { etag: tunnel.etag })
export const resumeTunnel = (tunnel: Tunnel) => change(`${tunnelPath(tunnel.id)}/resume`, "POST", { etag: tunnel.etag })
export const deleteTunnel = (tunnel: Tunnel) => change(tunnelPath(tunnel.id), "DELETE", { etag: tunnel.etag })
export const patchTunnel = (tunnel: Tunnel, patch: { name?: string; access_mode?: Tunnel["access_mode"] }) =>
  change<Tunnel>(tunnelPath(tunnel.id), "PATCH", { etag: tunnel.etag, body: patch })

export type RouteInput = {
  name: string
  host_match: TunnelRoute["host_match"]
  path_prefix: string | null
  origin: { scheme: TunnelRoute["origin"]["scheme"]; address: string; preserve_host: boolean }
}

/** Creates a route and returns its ID (from the route, or from the operation still applying it). */
export async function createRoute(tunnelId: string, input: RouteInput) {
  const result = await change<{ kind: string; id: string; resource_id?: string }>(
    `${tunnelPath(tunnelId)}/routes`,
    "POST",
    {
      body: { ...input, protocol: input.origin.scheme === "tcp" ? "tcp" : "http" },
    },
  )
  return result.kind === "operation" ? result.resource_id! : result.id
}
export const patchRoute = (
  route: TunnelRoute,
  patch: Partial<RouteInput> & { desired_state?: "active" | "disabled" },
) =>
  change(`${tunnelPath(route.tunnel_id)}/routes/${encodeURIComponent(route.id)}`, "PATCH", {
    etag: route.etag,
    body: patch,
  })
export const deleteRoute = (route: TunnelRoute) =>
  change(`${tunnelPath(route.tunnel_id)}/routes/${encodeURIComponent(route.id)}`, "DELETE", { etag: route.etag })

/** A route name from its hostname and path, readable in `pb tunnel route list`. */
export const routeName = (hostname: string | null, path: string | null) =>
  `${hostname ? hostname.replace(/^\*\./, "wildcard.") : "main"}${path ?? ""}`
    .replace(/[^A-Za-z0-9_.-]+/g, "-")
    .replace(/^[^A-Za-z0-9]+/, "")
    .slice(0, 63)

/** Deletes a route by ID, reading the ETag it needs. */
export async function removeRouteById(tunnelId: string, routeId: string) {
  const route = await api<TunnelRoute>(`${tunnelPath(tunnelId)}/routes/${encodeURIComponent(routeId)}`)
  await deleteRoute(route)
}

export const addDomain = (tunnelId: string, hostname: string, routeId: string) =>
  change(`${tunnelPath(tunnelId)}/domains`, "POST", { body: { hostname, route_id: routeId } })
export const verifyDomain = (domain: TunnelDomain) =>
  change(`${tunnelPath(domain.tunnel_id)}/domains/${encodeURIComponent(domain.id)}/verify`, "POST", {
    etag: domain.etag,
  })
export const removeDomain = (domain: TunnelDomain) =>
  change(`${tunnelPath(domain.tunnel_id)}/domains/${encodeURIComponent(domain.id)}`, "DELETE", { etag: domain.etag })

/** What creating a preview or tunnel answers: the resource, or the operation still making it. */
export type OperationOrResource = { kind: string; id: string; resource_id?: string }
export const resourceId = (result: OperationOrResource) =>
  result.kind === "operation" ? result.resource_id! : result.id

/** The host a resource answers on, without the scheme, for display. */
export const hostOf = (url: string) => url.replace(/^[a-z]+:\/\//, "").replace(/\/$/, "")
export const urlOf = (host: string) => (/^[a-z]+:\/\//.test(host) ? host : `https://${host}`)

/** Where traffic goes on the machine, written the way people type it: `localhost:3000`. */
export function originLabel(origin: { scheme: string; address: string }) {
  if (origin.scheme === "unix") return `unix:${origin.address}`
  // A shared file: the machine serves the file itself (`shareFile`).
  if (origin.scheme === "file") return origin.address.split("/").pop() ?? origin.address
  const address = origin.address.replace(/^127\.0\.0\.1(?=:)/, "localhost")
  return origin.scheme === "http" ? address : `${origin.scheme}://${address}`
}
