"use client"

import { PlugsIcon } from "@phosphor-icons/react"
import Link from "next/link"
import { useDeferredValue, useState } from "react"
import { useApiQuery, useWorkspace, type Machine } from "@/lib/api"
import { cursorPageSize } from "@/lib/cursor-pages"
import { hostOf, tunnelsPageQuery, type Tunnel } from "@/lib/tunnels"
import { usePageParam } from "@/lib/use-page-param"
import { useLastCount } from "@/lib/last-count"
import { useWorkspaceId } from "@/lib/workspace"
import { ListPager } from "@/components/list-pager"
import { NewTunnelButton } from "@/components/tunnels/new-tunnel"
import { RememberedEmpty } from "@/components/remembered-empty"
import { useTunnelActions } from "@/components/tunnels/tunnel-actions"
import { TunnelStatus } from "@/components/tunnels/tunnel-status"
import { SearchField, useListScope } from "@/components/list-toolbar"
import { useCurrentMachine } from "@/components/machine-shell"
import { RowMenus } from "@/components/row-menus"
import { Badge } from "@/components/ui/badge"
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { LoadError } from "@/components/load-error"
import { TableRowsSkeleton } from "@/components/skeletons"

/** The machine page's Tunnels tab. */
export function MachineTunnels() {
  return <TunnelList machine={useCurrentMachine()} />
}

/**
 * Lasting addresses for services on machines (Cloudflare Tunnel-style): each tunnel keeps its
 * address through restarts, and its page manages routes, domains and access. Paused tunnels stay
 * in the list; they keep everything and only stop taking traffic.
 */
export function TunnelList({ machine: tabMachine }: { machine?: Machine }) {
  const workspace = useWorkspace()
  const { scope, machine, aliasOf } = useListScope(tabMachine)
  const [search, setSearch] = useState("")
  const q = useDeferredValue(search.trim())
  const pages = usePageParam(`${q}|${scope ?? ""}`)
  const query = useApiQuery({
    ...tunnelsPageQuery({ workspace: workspace?.id ?? "", q, page: pages.page, machine: scope ?? undefined }),
    enabled: Boolean(workspace),
  })
  const shown = query.data?.items ?? []
  // The cookie's workspace: known in the browser before the session is (the prerendered shell).
  const workspaceId = useWorkspaceId()
  const lastRows = useLastCount(`tunnels:${workspaceId}:${scope ?? ""}`, query.data ? shown.length : undefined)

  // Whether the list was empty last time: an empty list loads as its empty state, not as a table.
  const lastEmpty = useLastCount(
    `tunnels-empty:${workspaceId}:${scope ?? ""}`,
    query.isSuccess ? Number(query.data.total === 0) : undefined,
  )
  const empty = (
    <Empty className="py-16">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <PlugsIcon />
        </EmptyMedia>
        <EmptyTitle>No tunnels yet</EmptyTitle>
        <EmptyDescription>
          Give a service on {machine ? machine.alias : "a machine"} a lasting address.
        </EmptyDescription>
      </EmptyHeader>
      <EmptyContent>
        <NewTunnelButton machine={machine} />
      </EmptyContent>
    </Empty>
  )
  if ((query.isSuccess ? query.data.total === 0 : query.isPending && lastEmpty === 1) && !q) return empty
  const remembered = lastEmpty === undefined && query.isPending && !q

  const list = (
    <section className="flex flex-col gap-3">
      <div className="flex items-center gap-2">
        <SearchField value={search} onChange={setSearch} placeholder="Search tunnels" />
        <div className="ml-auto">
          <NewTunnelButton machine={machine} />
        </div>
      </div>

      <div>
        <table className="w-full table-fixed text-sm">
          <colgroup>
            <col className="w-[24%]" />
            <col className="hidden lg:table-column" />
            {!scope && <col className="hidden w-[16%] md:table-column" />}
            <col className="hidden w-[20%] sm:table-column" />
            <col className="w-12" />
          </colgroup>
          <thead className="border-b border-divider text-left text-xs text-muted-foreground">
            <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
              <th>Tunnel</th>
              <th className="hidden lg:table-cell">Address</th>
              {!scope && <th className="hidden md:table-cell">Machine</th>}
              <th className="hidden sm:table-cell">Status</th>
              <th className="w-12" aria-label="Actions" />
            </tr>
          </thead>
          <tbody className="divide-y divide-divider border-b border-divider">
            {query.isPending
              ? <TableRowsSkeleton
                  rows={lastRows}
                  columns={[
                    { bar: "w-36" },
                    { className: "hidden lg:table-cell", bar: "w-56" },
                    !scope && { className: "hidden md:table-cell", bar: "w-20" },
                    { className: "hidden sm:table-cell", bar: "w-16" },
                    { empty: true },
                  ]}
                />
              : shown.map((tunnel) => (
                  <Row
                    key={tunnel.id}
                    tunnel={tunnel}
                    machineName={scope ? undefined : (aliasOf(tunnel.created_by_host_id) ?? "")}
                  />
                ))}
          </tbody>
        </table>
        {query.data && <ListPager page={query.data.page} size={cursorPageSize} total={query.data.total} go={pages.go} />}
        {query.isError && <LoadError what="tunnels" error={query.error} retry={() => query.refetch()} />}
        {query.isSuccess && shown.length === 0 && (
          <p className="py-12 text-center text-sm text-muted-foreground">No tunnels match “{search.trim()}”</p>
        )}
      </div>
    </section>
  )
  return remembered ? (
    <RememberedEmpty name="tunnels-empty" empty={empty}>
      {list}
    </RememberedEmpty>
  ) : (
    list
  )
}

function Row({ tunnel, machineName }: { tunnel: Tunnel; machineName?: string }) {
  const { entries, dialogs } = useTunnelActions(tunnel)
  return (
    <RowMenus
      label={`Actions for ${tunnel.name}`}
      entries={entries}
      dialogs={dialogs}
      row={(actions) => (
        <tr className="group relative transition-colors hover:bg-surface data-popup-open:bg-surface [&>td]:h-14 [&>td]:px-3">
          <td>
            <div className="flex min-w-0 items-center gap-2">
              <Link
                href={`/tunnels/${encodeURIComponent(tunnel.id)}`}
                className="truncate-x font-medium trim-cap after:absolute after:inset-0"
              >
                {tunnel.name}
              </Link>
              {tunnel.access_mode === "private" && (
                <Badge variant="outline" className="font-normal text-muted-foreground trim-cap">
                  Only you
                </Badge>
              )}
            </div>
          </td>
          <td className="hidden truncate font-mono text-code text-muted-foreground lg:table-cell">
            {hostOf(tunnel.stable_endpoint)}
          </td>
          {machineName !== undefined && <td className="hidden truncate md:table-cell">{machineName}</td>}
          <td className="hidden whitespace-nowrap text-muted-foreground sm:table-cell">
            <TunnelStatus tunnel={tunnel} />
          </td>
          <td className="text-right">{actions}</td>
        </tr>
      )}
    />
  )
}
