"use client"

import { queryOptions, useQuery, type QueryKey, type UseQueryOptions, type UseQueryResult } from "@tanstack/react-query"
import { hashKey } from "@tanstack/react-query"
import { createContext, useContext, useSyncExternalStore } from "react"
import type { components, paths } from "@/lib/api-schema"
import { queryKeys } from "@/lib/query-keys"
import { usageParams, type UsagePeriod } from "@/lib/usage-period"
import { useWorkspaceId } from "@/lib/workspace"
import { withDeadline } from "@/lib/deadline"
import { sessionVerdict } from "@/lib/session-verdict"
import { fetchMachineList, type MachinePages } from "@/lib/machine-pages"

/** Error envelope returned by the control plane: `{ error: { code, message } }`. */
export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
    /** The error's `details`, for errors whose next step needs them (e.g. sso_review_required). */
    readonly details?: unknown,
  ) {
    super(message)
  }
}

/** How long a read may take before it fails as unanswered. */
const readTimeoutMs = 30_000

/**
 * A request that never got an answer (offline, a dropped connection) fails in the browser with
 * "Failed to fetch"; people are told what happened instead. Cancelled requests stay cancelled.
 */
export async function send(input: string, init?: RequestInit): Promise<Response> {
  // A read the server never answers would hang its page, so reads always have a deadline (a
  // caller's signal adds cancellation, it doesn't remove it); writes, streams and uploads don't.
  const reads = !init?.method || init.method === "GET"
  const signal = reads ? withDeadline(init?.signal, readTimeoutMs) : (init?.signal ?? undefined)
  try {
    return await fetch(input, { ...init, signal })
  } catch (error) {
    if (error instanceof DOMException && error.name === "AbortError") throw error
    if (error instanceof DOMException && error.name === "TimeoutError")
      throw new ApiError(0, "timeout", "Paperboat took too long to answer. Try again.")
    throw new ApiError(0, "network", "Couldn’t reach Paperboat. Check your connection and try again.")
  }
}

/**
 * After a 401, asks `/me` what it meant (sessionVerdict): returns when the session is valid, sends
 * an ended session to sign-in, and throws an outage as an error the page can retry.
 */
export async function confirmSession(): Promise<void> {
  const session = await send("/api/v1/me", { headers: { accept: "application/json" } })
  const verdict = sessionVerdict(session.status)
  if (verdict === "valid") return
  if (verdict === "ended") {
    const here = window.location.pathname + window.location.search
    // Sign-in is a route handler that redirects to WorkOS, so it needs a full navigation.
    // eslint-disable-next-line @next/next/no-location-assign-relative-destination
    window.location.assign(`/auth/sign-in?return_to=${encodeURIComponent(here)}`)
    throw new ApiError(401, "unauthenticated", "Your session ended.")
  }
  throw new ApiError(session.status, "unavailable", "Paperboat is temporarily unavailable. Try again.")
}

export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await send(`/api/v1/${path}`, {
    ...init,
    headers: { accept: "application/json", ...init?.headers },
  })
  if (response.status === 401) {
    // Unknown paths also answer 401 (NOTES.md): with the session confirmed, this one is missing.
    await confirmSession()
    throw new ApiError(404, "not_found", "This isn't available yet.")
  }
  const body = (await response.json().catch(() => null)) as {
    data?: T
    error?: { code?: string; message?: string; details?: unknown }
  } | null
  if (!response.ok || !body || body.data === undefined) {
    const code = body?.error?.code ?? "unavailable"
    throw new ApiError(
      response.status,
      code,
      sentences[code] ?? body?.error?.message ?? "Paperboat is temporarily unavailable.",
      body?.error?.details,
    )
  }
  return body.data
}

/** Errors the console words itself, because it knows what the person can do next. */
const sentences: Record<string, string> = {
  bandwidth_quota_exhausted:
    "This workspace's hosted bandwidth is used up. Direct machine-to-machine access still works. Add bandwidth or change plan under Billing, or wait for the next allowance period.",
  team_private_access_off:
    "The team has not allowed private access to its shared machines. Ask a team owner or admin to turn it on.",
  github_connection_in_use:
    "Config sync is using GitHub right now. Stop syncing config on your machines first, then disconnect.",
  transfer_too_large:
    "This file is over 2 GB. Switch File transfers to Unlimited in Account, then send it over a direct connection or your own relay.",
  file_transfer_path_not_allowed:
    "Unlimited transfers don’t use Paperboat’s relays. Connect the machines directly or add your own relay.",
  team_sso_required: "This team requires your company's SSO. Sign out and sign in with your work email.",
  team_suspended:
    "This team is suspended, so changes are paused. Ask the team owner to restore billing or resume the team.",
}

/** For actions the server answers without a body (200/202/204), such as closing a terminal. */
export async function apiAction(path: string, init: RequestInit): Promise<void> {
  const response = await send(`/api/v1/${path}`, {
    ...init,
    headers: { accept: "application/json", "idempotency-key": idempotencyKey(), ...init.headers },
  })
  if (response.ok) return
  const body = (await response.json().catch(() => null)) as { error?: { code?: string; message?: string } } | null
  throw new ApiError(
    response.status,
    body?.error?.code ?? "unavailable",
    body?.error?.message ?? "Paperboat is temporarily unavailable.",
  )
}

const subscribeNothing = () => () => {}

/**
 * A random request key. `crypto.randomUUID` exists only on HTTPS pages; `getRandomValues`
 * works everywhere, including the console opened by Tailscale IP during development.
 */
export function idempotencyKey(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(16))
  return Array.from(bytes, (byte) => byte.toString(16).padStart(2, "0")).join("")
}

/** Hashes of queries whose data the server rendered with (see `ServerData`). */
export const ServerQueries = createContext<ReadonlySet<string>>(new Set())

/**
 * useQuery for data the console fetches in the browser. Until hydration finishes it reports
 * loading, exactly as the server rendered; otherwise a response that lands before React
 * hydrates a boundary would make the client markup differ from the server's. Data the server
 * prefetched is the exception: both sides render it, so it shows from the first paint.
 */
export function useApiQuery<TQueryFnData, TError = Error, TData = TQueryFnData, TQueryKey extends QueryKey = QueryKey>(
  options: UseQueryOptions<TQueryFnData, TError, TData, TQueryKey>,
): UseQueryResult<TData, TError> {
  const hydrated = useSyncExternalStore(
    subscribeNothing,
    () => true,
    () => false,
  )
  const fromServer = useContext(ServerQueries).has(hashKey(options.queryKey))
  const query = useQuery(options)
  if (hydrated || fromServer) return query
  return {
    ...query,
    data: undefined,
    error: null,
    status: "pending",
    isPending: true,
    isError: false,
    isSuccess: false,
  } as UseQueryResult<TData, TError>
}

/** Server schemas, generated from server/docs/openapi.json (`bun run api:types`). */
type Schemas = components["schemas"]

/** The `data` of a path's successful JSON response, as the server documents it. */
export type Data<P extends keyof paths, M extends "get" | "post" | "put" | "patch" = "get"> = paths[P][M] extends {
  responses: { 200: { content: { "application/json": { data?: infer T } } } }
}
  ? NonNullable<T>
  : never

// Hand-written until the server types `runtime_versions` (PLAN.md Log, S1-14 Lane C request);
// then these become Schemas["Me"] and Schemas["Machine"].
export type Me = {
  id: string
  email: string
  display_name: string
  role: string
}

export type MachineState = "online" | "offline" | "disconnected" | "revoked" | "pending"

export type Machine = {
  id: string
  alias: string
  platform: string
  architecture: string
  workspace_root: string
  state: MachineState
  online: boolean
  ownership: string
  owner_account?: string
  owner_team_id?: string
  shared: boolean
  can_manage: boolean
  /** What you may do on this machine, such as "terminal". */
  permissions?: string[]
  assigned_ip?: string
  public_identity_key: string
  runtime_versions: Record<string, string> | null
  enrolled_at?: string
  last_seen_at?: string
  /** Per feature: turned on for this machine, and whether the machine reports it running. */
  capabilities?: Record<string, { configured: boolean; observed: boolean }>
  availability?: {
    desired_mode: string
    desired_version: number
    observed_mode?: string
    status: string
    host_service_version?: string
    host_service_scope?: string
    update_health: string
    update_rollbacks: number
  }
  runtime_diagnostics?: { connector_state: string; observed_at?: string }
  /** `tag:<name>` from the selected workspace's policy (absent on servers before machine tags). */
  tags?: string[]
  /** The machine's last update report (Lane Q, C4); null when it has reported none. */
  update?: UpdateObservation | null
}

export type MachineOverview = Data<"/v1/machines/overview">

export type Usage = Schemas["ResourceBandwidthHistory"]

const minute = 60_000

export const meQuery = queryOptions({
  queryKey: queryKeys.me,
  queryFn: () => api<Me>("me"),
  staleTime: 5 * minute,
})

/** The server's published URLs and domains (the discovery response every client reads). */
export type ClientConfiguration = Data<"/v1/client-configuration">

export const clientConfigQuery = queryOptions({
  queryKey: ["client-configuration"],
  queryFn: () => api<ClientConfiguration>("client-configuration"),
  staleTime: Infinity,
})

/**
 * `sso_required`/`sso_signed_in` (F19): a team that requires SSO you haven't signed in with lists
 * only its name until you do.
 */
export type Workspace = Schemas["Workspace"] & { sso_required?: boolean; sso_signed_in?: boolean }

/** Whether a team waits for an SSO sign-in before it can be opened. */
export const needsSso = (workspace: Workspace) => workspace.sso_required === true && workspace.sso_signed_in === false

const personal: Workspace = { id: "personal", name: "Personal", kind: "personal", role: "owner" }

export const workspacesQuery = queryOptions({
  queryKey: queryKeys.workspaces,
  queryFn: async () => (await api<Data<"/v1/workspaces">>("workspaces")).items,
  staleTime: 5 * minute,
})

type MachinePage = MachinePages<Machine>

const machinePage = (params: Record<string, string>) =>
  api<{ items: Machine[]; pagination: { total: number } }>(`machines?${new URLSearchParams(params)}`)

async function fetchMachines(params: Record<string, string>): Promise<MachinePage> {
  const page = await machinePage(params)
  return { items: page.items, total: page.pagination.total, complete: page.items.length >= page.pagination.total }
}

/** Every machine of the workspace, revoked ones included (bounded, see lib/machine-pages.ts). */
export const machinesQuery = (workspace: string) =>
  queryOptions({
    queryKey: queryKeys.machines(workspace),
    queryFn: async () => (await fetchMachineList(machinePage, workspace))!,
    refetchInterval: 15_000,
  })

export type MachineSummary = {
  /** Machines in use (revoked and disconnected ones are left out). */
  total: number
  by_state: { online: number; offline: number; pending: number; revoked: number } & Record<string, number>
  /** Machines in use per platform: linux, macos, windows, and any other the server reports. */
  by_platform: Record<string, number>
}

/** Workspace totals from the server (GET /v1/machines/summary), never counted from a loaded list. */
export const machineSummaryQuery = (workspace: string) =>
  queryOptions({
    queryKey: ["machines", workspace, "summary"],
    queryFn: () => api<MachineSummary>(`machines/summary?${new URLSearchParams({ workspace })}`),
    refetchInterval: 15_000,
  })

/** Server-side search by name or ID, for workspaces too large to list. */
export const machineSearchQuery = (workspace: string, q: string) =>
  queryOptions({
    queryKey: ["machines", workspace, "search", q],
    queryFn: () => fetchMachines({ limit: "20", workspace, q }),
    staleTime: 30_000,
  })

export const machineQuery = (id: string) =>
  queryOptions({
    queryKey: ["machines", "one", id],
    queryFn: () => api<Machine>(`machines/${encodeURIComponent(id)}`),
    refetchInterval: 15_000,
  })

/** Seat limits and the install command exist for the personal workspace only. */
export const machineOverviewQuery = queryOptions({
  queryKey: ["machines", "overview"],
  queryFn: () => api<MachineOverview>("machines/overview"),
})

export const usageQuery = (workspace: string, period: UsagePeriod) =>
  queryOptions({
    queryKey: queryKeys.usage(workspace, period),
    queryFn: () => api<Usage>(`usage?${usageParams(workspace, period)}`),
    staleTime: 5 * minute,
  })

/** Usage from `start` until now: the billing period, shared by the allowance and bandwidth cards. */
export const usageSinceQuery = (workspace: string, start: string) =>
  queryOptions({
    queryKey: ["usage-since", workspace, start],
    queryFn: () =>
      api<Usage>(`usage?${new URLSearchParams({ start, end: new Date().toISOString(), limit: "200", workspace })}`),
    enabled: Boolean(workspace && start),
    staleTime: 5 * minute,
  })

export type PathMode = "direct" | "relay" | "edge"

/** Whose infrastructure carried traffic: any, Paperboat's relays and edges, or self-hosted ones. */
export type TrafficSource = "all" | "managed" | "self_hosted"

export type TrafficBreakdown = {
  upload: number
  download: number
  byPath: Record<PathMode, number>
}

type PathTotal = Usage["totals"]["by_path"][number]

/**
 * Traffic for one source, split by path, from the server's whole-period aggregates. Direct
 * (peer-to-peer) traffic crosses no relay or edge, so it counts only for "all".
 */
export function trafficBreakdown(byPath: PathTotal[], source: TrafficSource): TrafficBreakdown {
  const result: TrafficBreakdown = { upload: 0, download: 0, byPath: { direct: 0, relay: 0, edge: 0 } }
  for (const path of byPath) {
    if (source !== "all" && path.hosting !== source) continue
    result.upload += path.upload_bytes
    result.download += path.download_bytes
    result.byPath[path.mode] += path.upload_bytes + path.download_bytes
  }
  return result
}

export type UpdateObservation = Schemas["UpdateObservation"]
export type UpdateState = UpdateObservation["state"]

/**
 * Starting an update is a maintenance approval the owner requests and then approves, the same
 * two calls `pb` makes. The machine performs it on its next maintenance check.
 */
export async function approveUpdate(workspace: string, machineId: string, targetVersion: string): Promise<void> {
  const machine = encodeURIComponent(machineId)
  const selector = `?workspace=${encodeURIComponent(workspace)}`
  const approval = await api<{ id: string }>(`machines/${machine}/maintenance-approvals${selector}`, {
    method: "POST",
    headers: { "content-type": "application/json", "idempotency-key": idempotencyKey() },
    body: JSON.stringify({ action: "update", target_version: targetVersion, reason: "Approved from the console" }),
  })
  await api(`machines/${machine}/maintenance-approvals/${encodeURIComponent(approval.id)}/approve${selector}`, {
    method: "POST",
  })
}

/** An edge serving the account's previews and tunnels. Self-hosted edges are not listed yet. */
export type Edge = Data<"/v1/edges">["items"][number]

export const edgesQuery = queryOptions({
  queryKey: ["edges"],
  queryFn: async () => (await api<Data<"/v1/edges">>("edges?limit=100")).items,
  staleTime: 60_000,
})

/** How long this machine took to reach an edge or relay, as the machine measured it. Display only. */
export type NodeLatency = Data<"/v1/machines/{machine_id}/network-latency">["items"][number]

export const machineLatencyQuery = (machineId: string) =>
  queryOptions({
    queryKey: ["machine-latency", machineId],
    queryFn: async () =>
      (
        await api<Data<"/v1/machines/{machine_id}/network-latency">>(
          `machines/${encodeURIComponent(machineId)}/network-latency`,
        )
      ).items,
    staleTime: 60_000,
    refetchInterval: 120_000,
  })

export function updateMachine(id: string, alias: string): Promise<Machine> {
  return api<Machine>(`machines/${encodeURIComponent(id)}`, {
    method: "PATCH",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ alias }),
  })
}

export function removeMachine(id: string): Promise<unknown> {
  return api(`machines/${encodeURIComponent(id)}`, { method: "DELETE" })
}

/** Who the machine belongs to, from the signed-in account's point of view. */
export function machineOwner(machine: Machine, me: Me | undefined): "you" | "team" | "shared" | undefined {
  if (machine.owner_team_id) return "team"
  if (!me || !machine.owner_account) return
  return machine.owner_account === me.id ? "you" : "shared"
}

/** The selected workspace, falling back to Personal when the saved one is no longer available. */
export function useWorkspace(): Workspace | undefined {
  const id = useWorkspaceId()
  const { data, isError } = useApiQuery(workspacesQuery)
  // Personal always exists, so a failed workspace list must not block the rest of the console.
  if (isError) return personal
  return data?.find((workspace) => workspace.id === id) ?? data?.find((workspace) => workspace.kind === "personal")
}

/** The workspace's machines you can use: every page, revoked ones left out. */
export function useMachines() {
  const workspace = useWorkspace()
  return useApiQuery({
    ...machinesQuery(workspace?.id ?? ""),
    enabled: Boolean(workspace),
    select: (page: MachinePage) => page.items.filter((machine) => machine.state !== "revoked"),
  })
}

/** One machine: from the loaded page when it is there, otherwise fetched on its own. */
export function useMachine(id: string | null | undefined) {
  const machines = useMachines()
  const listed = id ? machines.data?.find((machine) => machine.id === id) : undefined
  const single = useApiQuery({ ...machineQuery(id ?? ""), enabled: Boolean(id) && machines.isSuccess && !listed })
  if (!id || listed || machines.isPending || machines.isError) return { ...machines, data: listed }
  return single
}

/** A team as `GET /v1/teams/{team_id}` returns it. */
export type Team = Data<"/v1/teams/{team_id}">

export type RunningCounts = { terminals: number; tunnels: number; previews: number }

/** Every page of a cursor collection, bounded so one huge account cannot stall the table. */
/** A cursor-paged list: `complete` is false when the bound below stopped it short. */
export type Paged<T> = { items: T[]; complete: boolean }

/** Pages of 200 by cursor, at most 10 (2,000 items), so a large workspace can't stall a tab. */
export async function allPages<T>(path: string, params: Record<string, string> = {}): Promise<Paged<T>> {
  const items: T[] = []
  let cursor = ""
  for (let page = 0; page < 10; page++) {
    const query = new URLSearchParams({ ...params, limit: "200", ...(cursor && { cursor }) })
    const result = await api<{ items: T[]; next_cursor?: string }>(`${path}?${query}`)
    items.push(...result.items)
    if (!result.next_cursor) return { items, complete: true }
    cursor = result.next_cursor
  }
  return { items, complete: false }
}

/**
 * What each machine is running: open terminal sessions, active tunnels and active previews, from
 * the server's per-machine counts (one computation serves every tab, at most 10 s old). Machines
 * running nothing are left out; when the server stopped short (`truncated`), a machine it left out
 * is unknown rather than idle.
 */
export const runningQuery = (workspace: string) =>
  queryOptions({
    queryKey: ["running", workspace],
    queryFn: async () => {
      const data = await api<{ items: ({ machine_id: string } & RunningCounts)[]; truncated: boolean }>(
        `machines/running-counts?${new URLSearchParams({ workspace })}`,
      )
      const counts = new Map(
        data.items.map(({ machine_id, terminals, tunnels, previews }) => [machine_id, { terminals, tunnels, previews }]),
      )
      return { counts, truncated: data.truncated }
    },
    refetchInterval: 30_000,
  })

export function useRunning() {
  const workspace = useWorkspace()
  return useApiQuery({ ...runningQuery(workspace?.id ?? ""), enabled: Boolean(workspace) })
}
