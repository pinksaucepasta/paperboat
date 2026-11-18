"use client"

import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { ErrorBlock } from "@/components/error-block"
import { pageSize, type CursorList } from "@/lib/paging"
import { cn } from "@/lib/utils"

export type Column<R> = { header: string; cell: (row: R) => React.ReactNode; className?: string }

type Page = { total?: number; total_capped?: boolean; next_after?: string; next_cursor?: string; next?: string; pagination?: { total: number; next_offset: number | null } }

/** "51–100 of N" with Previous/Next; N is the server's total (100000+ when it stopped counting). */
export function PagerBar({ page, count, total, capped, hasNext, go }: { page: number; count: number; total?: number; capped?: boolean; hasNext: boolean; go: (page: number) => void }) {
  if (page === 1 && !hasNext) return total !== undefined && count > 0 ? <p className="py-3 text-right text-sm text-muted-foreground tabular-nums" data-list-range>{total} total</p> : null
  const from = (page - 1) * pageSize
  const range = `${from + 1}–${from + count}`
  return (
    <nav className="flex items-center justify-end gap-3 py-3 text-sm text-muted-foreground" aria-label="Pages">
      <span className="tabular-nums" data-list-range>
        {range}
        {total !== undefined && ` of ${total}${capped ? "+" : ""}`}
      </span>
      <Button variant="outline" size="sm" disabled={page === 1} onClick={() => go(page - 1)}>
        Previous
      </Button>
      <Button variant="outline" size="sm" disabled={!hasNext} onClick={() => go(page + 1)}>
        Next
      </Button>
    </nav>
  )
}

/**
 * A server-paged table with every state: loading (rows shaped like the loaded ones), error (message, reference,
 * Retry), empty, and data with the pager. `rows` picks the page's items out of the response.
 */
export function DataTable<P extends Page, R>({
  what,
  list,
  rows,
  columns,
  rowKey,
  empty,
}: {
  what: string
  list: CursorList<P>
  rows: (data: P) => R[]
  columns: Column<R>[]
  rowKey: (row: R) => string
  empty: string
}) {
  if (list.isError) return <ErrorBlock what={what} error={list.error} retry={() => list.refetch()} />
  const items = list.data ? rows(list.data) : []
  return (
    <div data-table={what} data-state={list.isPending ? "loading" : items.length === 0 ? "empty" : "ready"}>
      <div className="overflow-x-auto">
        <table className="w-full border-collapse md:min-w-[40rem] text-sm">
          <thead>
            <tr className="border-b border-divider text-left text-xs text-muted-foreground">
              {columns.map((c) => (
                <th key={c.header} scope="col" className={cn("h-9 px-3 font-normal", c.className)}>
                  {c.header}
                </th>
              ))}
            </tr>
          </thead>
          <tbody className="divide-y divide-divider">
            {list.isPending
              ? Array.from({ length: 8 }, (_, i) => (
                  <tr key={i} className="[&>td]:h-14 [&>td]:px-3">
                    {columns.map((c) => (
                      <td key={c.header} className={c.className}>
                        <Skeleton className="h-3 w-24 max-w-full" />
                      </td>
                    ))}
                  </tr>
                ))
              : items.map((row) => (
                  <tr key={rowKey(row)} className="hover:bg-muted/40 [&>td]:h-14 [&>td]:px-3">
                    {columns.map((c) => (
                      <td key={c.header} className={cn("max-w-[24rem] truncate", c.className)}>
                        {c.cell(row)}
                      </td>
                    ))}
                  </tr>
                ))}
          </tbody>
        </table>
      </div>
      {!list.isPending && items.length === 0 && <p className="py-12 text-center text-sm text-muted-foreground">{empty}</p>}
      {list.data && (
        <PagerBar page={list.page} count={items.length} total={list.data.total ?? list.data.pagination?.total} capped={list.data.total_capped} hasNext={list.hasNext} go={list.go} />
      )}
    </div>
  )
}
