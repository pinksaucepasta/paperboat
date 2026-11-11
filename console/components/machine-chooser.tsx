"use client"

import Link from "next/link"
import { useRouter } from "next/navigation"
import { useDeferredValue, useEffect, useState } from "react"
import { useMachines } from "@/lib/api"
import { formatPlatform } from "@/lib/format"
import { useLastCount } from "@/lib/last-count"
import { useTimeFormat } from "@/lib/use-time-format"
import { cn } from "@/lib/utils"
import { useWorkspaceId } from "@/lib/workspace"
import { SearchField } from "@/components/list-toolbar"
import { LoadError } from "@/components/load-error"
import { StatusDot } from "@/components/machine-status"
import { TableRowsSkeleton } from "@/components/skeletons"

/**
 * For pages that show one machine at a time (Files): the workspace's machines to pick from,
 * online first, in the machines table's row pattern (name, then status dot; offline ones say since
 * when under the name). Each row opens `href(id)`, the same URL the header's machine picker sets; offline
 * machines are listed too (the page says what it can't reach). Arrow keys move between rows and
 * Enter opens one. A workspace with one machine goes straight to it. `action` sits at the right of the search field.
 */
export function MachineChooser({ href, action }: { href: (id: string) => string; action?: React.ReactNode }) {
  const router = useRouter()
  const machines = useMachines()
  const { date } = useTimeFormat()
  const [search, setSearch] = useState("")
  const q = useDeferredValue(search.trim().toLowerCase())
  const lastRows = useLastCount(`machine-chooser:${useWorkspaceId()}`, machines.data?.length)
  const only = machines.data?.length === 1 ? machines.data[0].id : undefined
  useEffect(() => {
    if (only) router.replace(href(only))
  }, [only, href, router])

  const shown = (machines.data ?? [])
    .filter((machine) => !q || machine.alias.toLowerCase().includes(q))
    .sort((a, b) => Number(b.online) - Number(a.online) || a.alias.localeCompare(b.alias))

  // Up and down move focus between the rows' links; Enter follows the focused one.
  const move = (event: React.KeyboardEvent) => {
    if (event.key !== "ArrowDown" && event.key !== "ArrowUp") return
    const links = [...event.currentTarget.querySelectorAll<HTMLAnchorElement>("a[data-machine]")]
    if (!links.length) return
    event.preventDefault()
    const at = links.indexOf(document.activeElement as HTMLAnchorElement)
    const next = event.key === "ArrowDown" ? Math.min(at + 1, links.length - 1) : Math.max(at - 1, 0)
    links[at === -1 ? 0 : next].focus()
  }

  return (
    <section className="flex flex-col gap-3" onKeyDown={move}>
      <div className="flex items-center gap-2">
        <SearchField value={search} onChange={setSearch} placeholder="Search machines" />
        {action && <div className="ml-auto flex items-center gap-2">{action}</div>}
      </div>
      <table className="w-full table-fixed text-sm">
        <colgroup>
          <col />
          <col className="hidden w-[30%] sm:table-column" />
        </colgroup>
        <thead className="border-b border-divider text-left text-xs text-muted-foreground">
          <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
            <th>Machine</th>
            <th className="hidden sm:table-cell">System</th>
          </tr>
        </thead>
        <tbody className="divide-y divide-divider border-b border-divider">
          {machines.isPending ? (
            <TableRowsSkeleton
              rows={lastRows ?? 6}
              columns={[
                { bar: "w-36", dot: true },
                { className: "hidden sm:table-cell", bar: "w-24" },
              ]}
            />
          ) : (
            shown.map((machine) => (
              <tr
                key={machine.id}
                className="group relative transition-colors focus-within:bg-surface hover:bg-surface [&>td]:h-14 [&>td]:px-3"
              >
                <td>
                  <div className="flex h-[1lh] min-w-0 items-center gap-2">
                    <Link
                      href={href(machine.id)}
                      data-machine
                      className={cn(
                        "truncate-x font-medium outline-none trim-cap after:absolute after:inset-0",
                        !machine.online && "text-muted-foreground",
                      )}
                    >
                      {machine.alias}
                    </Link>
                    <StatusDot online={machine.online} />
                  </div>
                  {!machine.online && (
                    <div className="truncate text-xs text-muted-foreground">
                      {machine.last_seen_at ? `Offline · since ${date(machine.last_seen_at, "day")}` : "Offline"}
                    </div>
                  )}
                </td>
                <td className="hidden truncate text-muted-foreground sm:table-cell">
                  {formatPlatform(machine.platform)} · {machine.architecture}
                </td>
              </tr>
            ))
          )}
        </tbody>
      </table>
      {machines.isError && <LoadError what="machines" error={machines.error} retry={() => machines.refetch()} />}
      {machines.isSuccess && shown.length === 0 && (
        <p className="py-12 text-center text-sm text-muted-foreground">
          {q ? `No machines match “${search.trim()}”` : "No machines in this workspace yet"}
        </p>
      )}
    </section>
  )
}
