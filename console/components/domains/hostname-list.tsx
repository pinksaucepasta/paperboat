"use client"

import Link from "next/link"
import { useDeferredValue, useState } from "react"
import { useApiQuery, useWorkspace } from "@/lib/api"
import { cursorPageSize } from "@/lib/cursor-pages"
import { domainsPageQuery, type DomainItem } from "@/lib/domain-list"
import { hostOf } from "@/lib/tunnels"
import { usePageParam } from "@/lib/use-page-param"
import { domainStates } from "@/components/tunnels/tunnel-hostnames"
import { ListPager } from "@/components/list-pager"
import { SearchField } from "@/components/list-toolbar"

/** Who publishes a hostname: the tunnel (linked) or the preview (its address label). */
export function HostnameOwner({ item }: { item: DomainItem }) {
  if (item.kind === "tunnel")
    return (
      <Link
        href={`/tunnels/${encodeURIComponent(item.domain.tunnel_id ?? "")}`}
        className="truncate text-muted-foreground underline-offset-4 hover:text-foreground hover:underline"
      >
        {item.domain.owner_name ?? "Tunnel"}
      </Link>
    )
  return (
    <span className="truncate font-mono text-code text-muted-foreground">
      {item.domain.endpoint ? hostOf(item.domain.endpoint).split(".")[0] : "Preview"}
    </span>
  )
}

/**
 * Every hostname your tunnels and live previews publish, one server page at a time (GET
 * /v1/domains): searched and counted by the server, with Previous/Next and the page in the URL.
 */
export function HostnameList() {
  const workspace = useWorkspace()
  const [search, setSearch] = useState("")
  const q = useDeferredValue(search.trim())
  const pages = usePageParam(q)
  const list = useApiQuery({
    ...domainsPageQuery({ workspace: workspace?.id ?? "", q, page: pages.page }),
    enabled: Boolean(workspace),
  })
  const items = list.data?.items ?? []
  if (list.data && list.data.total === 0 && !q) return null
  return (
    <section className="flex flex-col gap-3" data-hostnames>
      <div className="flex items-center gap-2">
        <h2 className="text-sm font-medium">Tunnel and preview hostnames</h2>
        <div className="ml-auto">
          <SearchField value={search} onChange={setSearch} placeholder="Search hostnames" />
        </div>
      </div>
      <table className="w-full table-fixed text-sm">
        <colgroup>
          <col />
          <col className="w-[28%]" />
          <col className="w-[22%]" />
        </colgroup>
        <thead className="border-b border-divider text-left text-xs text-muted-foreground">
          <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
            <th>Hostname</th>
            <th>Used by</th>
            <th>Status</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-divider border-b border-divider">
          {items.map((item) => (
            <tr key={`${item.kind}/${item.domain.id}`} className="[&>td]:h-12 [&>td]:px-3">
              <td className="truncate font-mono text-code">{item.domain.hostname}</td>
              <td>
                <HostnameOwner item={item} />
              </td>
              <td className="text-muted-foreground">
                {item.kind === "tunnel" ? (domainStates[item.domain.state]?.label ?? item.domain.state) : "Live"}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {list.data && <ListPager page={list.data.page} size={cursorPageSize} total={list.data.total} go={pages.go} />}
      {list.data && list.data.total === 0 && (
        <p className="py-8 text-center text-sm text-muted-foreground">No hostnames match “{q}”</p>
      )}
      {list.isError && <p className="py-8 text-center text-sm text-muted-foreground">Couldn’t load hostnames.</p>}
    </section>
  )
}
