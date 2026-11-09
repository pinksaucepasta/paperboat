import "server-only"

import { dehydrate, QueryClient, type DehydratedState } from "@tanstack/react-query"
import { cookies, headers } from "next/headers"
import { connection } from "next/server"
import { cache } from "react"
import { viewerFrom, type Viewer } from "@/lib/format"
import { queryKeys } from "@/lib/query-keys"
import { controlPlaneFetch } from "@/lib/server/control-plane"

const workspacePattern = /^[a-z0-9-]{1,63}$/

/**
 * Reads the control plane with the visitor's cookie, for the selected workspace. Anything that fails
 * is simply left out; the browser fetches it as before and handles errors and sign-in.
 */
const request = cache(async () => {
  // Per-request data: never part of a prerendered shell.
  await connection()
  const jar = await cookies()
  const selected = jar.get("pb-workspace")?.value
  const workspace = selected && workspacePattern.test(selected) ? selected : "personal"
  const cookie = jar.toString()

  const get = async <T>(path: string): Promise<T | undefined> => {
    try {
      const response = await controlPlaneFetch(`/v1/${path}`, {
        headers: { accept: "application/json", cookie },
        signal: AbortSignal.timeout(3_000),
      })
      if (!response.ok) return
      const body = (await response.json()) as { data?: T }
      return body.data
    } catch {
      return
    }
  }
  // The browser keeps its time zone in pb-tz (Providers); with the locale it lets the server write dates as the browser will.
  const viewer = viewerFrom(jar.get("pb-tz")?.value, (await headers()).get("accept-language"))
  return { workspace, get, viewer }
})

type Session = {
  me?: unknown
  workspaces?: { items: { id: string; kind: string }[] }
  billing?: unknown
  products?: { items: unknown[] }
}

/**
 * What every page shows (the user menu, the workspace crumb, the quota banner), fetched once per
 * request. Billing is read in parallel by the selected id; it is kept only when the list confirms
 * that workspace (a stale cookie falls back to personal in the browser, which then fetches its own).
 */
const session = cache(async () => {
  const { workspace, get } = await request()
  const billingPath =
    workspace === "personal"
      ? "billing/workspaces/personal"
      : `billing/workspaces/teams/${encodeURIComponent(workspace)}`
  const [me, workspaces, billing, products] = await Promise.all([
    get("me"),
    get<NonNullable<Session["workspaces"]>>("workspaces"),
    get(billingPath),
    get<{ items: unknown[] }>(`${billingPath}/products`),
  ])
  const confirmed = workspaces?.items.some((item) => item.id === workspace)
  return confirmed ? { workspace, me, workspaces, billing, products } : { workspace, me, workspaces }
})

function seed(queryClient: QueryClient, workspace: string, { me, workspaces, billing, products }: Session) {
  if (me) queryClient.setQueryData(queryKeys.me, me)
  if (workspaces) queryClient.setQueryData(queryKeys.workspaces, workspaces.items)
  if (billing) queryClient.setQueryData(queryKeys.billing(workspace), billing)
  if (products) queryClient.setQueryData(queryKeys.billingProducts(workspace), products.items)
}

/** A page's own first read: its query key, its control-plane path, and the field the browser query keeps. */
export type PageRead = { key: readonly unknown[]; path: string; field?: string }

/** The session data, plus a page's own reads (see `WithSession`), for every console page. */
export async function prefetchSession(
  reads?: (workspace: string) => PageRead[],
): Promise<{ state: DehydratedState; workspace: string; viewer?: Viewer }> {
  const { workspace, get, viewer } = await request()
  const list = reads?.(workspace) ?? []
  const [data, ...results] = await Promise.all([
    session(),
    ...list.map((read) => get<Record<string, unknown>>(read.path)),
  ])
  const queryClient = new QueryClient()
  seed(queryClient, workspace, data)
  list.forEach((read, index) => {
    const result = results[index]
    if (result) queryClient.setQueryData(read.key, read.field ? result[read.field] : result)
  })
  return { state: dehydrate(queryClient), workspace, viewer }
}

/**
 * Fetches what the machines page shows first, so its HTML arrives filled in (as Tailscale's does)
 * instead of loading in the browser afterwards.
 */
export async function prefetchMachinesPage(): Promise<{ state: DehydratedState; workspace: string }> {
  const { workspace, get } = await request()
  const queryClient = new QueryClient()

  // Not usage: it takes ~1.7 s and would hold the whole page back; the bandwidth card loads it.
  const [data, machines, overview, summary] = await Promise.all([
    session(),
    // The Machines table's first page (lib/machine-list.ts), not every machine.
    get<{ items: unknown[]; total: number }>(`machines?${new URLSearchParams({ workspace, limit: "50" })}`),
    get("machines/overview"),
    get(`machines/summary?${new URLSearchParams({ workspace })}`),
  ])

  // Shapes match the browser query functions in lib/api.ts.
  seed(queryClient, workspace, data)
  if (overview) queryClient.setQueryData(queryKeys.machineOverview, overview)
  if (summary) queryClient.setQueryData(queryKeys.machineSummary(workspace), summary)
  if (machines)
    queryClient.setQueryData(queryKeys.machineList(workspace, "", {}, 1), {
      items: machines.items,
      total: machines.total,
      page: 1,
    })
  return { state: dehydrate(queryClient), workspace }
}
