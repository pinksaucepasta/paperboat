import "server-only"

import { dehydrate, QueryClient, type DehydratedState } from "@tanstack/react-query"
import { cookies } from "next/headers"
import { connection } from "next/server"
import { cache } from "react"
import { defaultUsagePeriod, usageParams } from "@/lib/usage-period"
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
  return { workspace, get }
})

/** The session data every page shows (the user menu, the workspace crumb), fetched once per request. */
const session = cache(async () => {
  const { workspace, get } = await request()
  const [me, workspaces] = await Promise.all([get("me"), get<{ items: unknown[] }>("workspaces")])
  return { workspace, me, workspaces }
})

function seed(queryClient: QueryClient, me: unknown, workspaces: { items: unknown[] } | undefined) {
  if (me) queryClient.setQueryData(queryKeys.me, me)
  if (workspaces) queryClient.setQueryData(queryKeys.workspaces, workspaces.items)
}

/** The session data alone, for every console page (see `WithSession`). */
export async function prefetchSession(): Promise<{ state: DehydratedState; workspace: string }> {
  const { workspace, me, workspaces } = await session()
  const queryClient = new QueryClient()
  seed(queryClient, me, workspaces)
  return { state: dehydrate(queryClient), workspace }
}

/**
 * Fetches what the machines page shows first, so its HTML arrives filled in (as Tailscale's does)
 * instead of loading in the browser afterwards.
 */
export async function prefetchMachinesPage(): Promise<{ state: DehydratedState; workspace: string }> {
  const { workspace, get } = await request()
  const queryClient = new QueryClient()

  const [{ me, workspaces }, machines, usage] = await Promise.all([
    session(),
    // The Machines table's first page (lib/machine-list.ts), not every machine.
    get<{ items: unknown[]; total: number }>(`machines?${new URLSearchParams({ workspace, limit: "50" })}`),
    get(`usage?${usageParams(workspace, defaultUsagePeriod)}`),
  ])

  // Shapes match the browser query functions in lib/api.ts.
  seed(queryClient, me, workspaces)
  if (machines)
    queryClient.setQueryData(queryKeys.machineList(workspace, "", {}, 1), {
      items: machines.items,
      total: machines.total,
      page: 1,
    })
  if (usage) queryClient.setQueryData(queryKeys.usage(workspace, defaultUsagePeriod), usage)
  return { state: dehydrate(queryClient), workspace }
}
