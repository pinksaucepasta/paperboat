import "server-only"

import { dehydrate, QueryClient, type DehydratedState } from "@tanstack/react-query"
import { cookies } from "next/headers"
import { connection } from "next/server"
import { defaultUsagePeriod, usageParams } from "@/lib/usage-period"
import { queryKeys } from "@/lib/query-keys"
import { controlPlaneFetch } from "@/lib/server/control-plane"

const workspacePattern = /^[a-z0-9-]{1,63}$/

/**
 * Fetches, with the visitor's session, what the machines page shows first, so its HTML arrives
 * filled in (as Tailscale's does) instead of loading in the browser afterwards. Anything that
 * fails is simply left out; the browser fetches it as before and handles errors and sign-in.
 */
export async function prefetchMachinesPage(): Promise<{ state: DehydratedState; workspace: string }> {
  // Per-request data: never part of a prerendered shell.
  await connection()
  const jar = await cookies()
  const selected = jar.get("pb-workspace")?.value
  const workspace = selected && workspacePattern.test(selected) ? selected : "personal"
  const cookie = jar.toString()
  const queryClient = new QueryClient()

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

  const [me, workspaces, machines, usage] = await Promise.all([
    get("me"),
    get<{ items: unknown[] }>("workspaces"),
    // The Machines table's first page (lib/machine-list.ts), not every machine.
    get<{ items: unknown[]; total: number }>(`machines?${new URLSearchParams({ workspace, limit: "50" })}`),
    get(`usage?${usageParams(workspace, defaultUsagePeriod)}`),
  ])

  // Shapes match the browser query functions in lib/api.ts.
  if (me) queryClient.setQueryData(queryKeys.me, me)
  if (workspaces) queryClient.setQueryData(queryKeys.workspaces, workspaces.items)
  if (machines)
    queryClient.setQueryData(queryKeys.machineList(workspace, "", {}, 1), {
      items: machines.items,
      total: machines.total,
      page: 1,
    })
  if (usage) queryClient.setQueryData(queryKeys.usage(workspace, defaultUsagePeriod), usage)
  return { state: dehydrate(queryClient), workspace }
}
