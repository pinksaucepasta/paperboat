"use client"

import { InfoIcon, LaptopIcon, MagnifyingGlassIcon, XIcon } from "@phosphor-icons/react"
import Link from "next/link"
import { useDeferredValue, useState } from "react"
import {
  useApiQuery,
  machineOwner,
  machineSummaryQuery,
  meQuery,
  useRunning,
  useWorkspace,
  type Machine,
  type Me,
  type RunningCounts,
} from "@/lib/api"
import { formatPlatform } from "@/lib/format"
import { cursorPageSize } from "@/lib/cursor-pages"
import { parseQuery, type FilterKey } from "@/lib/machine-filter"
import { machineListQuery } from "@/lib/machine-list"
import { tagsLive, tagsQuery } from "@/lib/policy"
import { useLastCount } from "@/lib/last-count"
import { useWorkspaceId } from "@/lib/workspace"
import { ListPager } from "@/components/list-pager"
import { TagChips } from "@/components/machine-tags"
import { AddMachineDialog } from "@/components/add-machine-dialog"
import { MachineRowMenus } from "@/components/machine-actions"
import { useMachineQuery, useMachineUrl } from "@/components/machine-query"
import { MachineAddressStack } from "@/components/machine-address"
import { FilterButton, FilterChip } from "@/components/machine-filters"
import { MachineUpdate } from "@/components/machine-update"
import { MachineDot, machineVersion } from "@/components/machine-status"
import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group"
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { Skeleton } from "@/components/ui/skeleton"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { LoadError } from "@/components/load-error"
import { TableRowsSkeleton } from "@/components/skeletons"
import { SharedName } from "@/components/view-transitions"

export function MachineTable() {
  const { data: me } = useApiQuery(meQuery)
  const { data: running, isError: runningFailed } = useRunning()
  const workspace = useWorkspace()
  const [query, setQuery] = useMachineQuery()
  const parsed = parseQuery(useDeferredValue(query))
  const keys: FilterKey[] = [
    "status",
    "os",
    ...(tagsLive ? (["tag"] as const) : []),
    ...(workspace?.kind === "personal" ? (["owner"] as const) : []),
  ]
  const workspaceTags = useApiQuery(tagsQuery(workspace?.id ?? ""))
  const tags = Object.fromEntries(
    (workspaceTags.data?.tags ?? []).map((tag) => tag.name.replace(/^tag:/, "")).map((name) => [name, name]),
  )
  const applied = (Object.entries(parsed.filters) as [FilterKey, string][]).filter(([key]) => keys.includes(key))
  const filtered = Boolean(parsed.text) || applied.length > 0
  const pages = usePages(query)
  const list = useApiQuery({
    ...machineListQuery(workspace?.id ?? "", parsed.text, Object.fromEntries(applied), pages.page),
    enabled: Boolean(workspace),
  })
  const { isPending, isError, error, refetch } = list
  // The cards' count (already loaded with them) sizes the loading rows.
  const summary = useApiQuery({ ...machineSummaryQuery(workspace?.id ?? ""), enabled: Boolean(workspace) })
  const shown = list.data?.items
  // The cookie's workspace: known in the browser before the session is (the prerendered shell).
  const workspaceId = useWorkspaceId()
  const lastRows = useLastCount(`machines:${workspaceId}:${query}:${pages.page}`, shown?.length)
  const total = list.data?.total ?? 0

  return (
    <section id="machines-table" className="flex scroll-mt-16 flex-col gap-3">
      <div className="flex items-center gap-2">
        <InputGroup className="h-8 max-w-sm border-transparent bg-surface dark:bg-surface">
          {/* Same type and icon size as the header's search, so the two read as one control. */}
          <InputGroupAddon>
            <MagnifyingGlassIcon className="size-4" />
          </InputGroupAddon>
          <InputGroupInput
            className="md:text-sm"
            placeholder="Search machines"
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            spellCheck={false}
          />
          {query && (
            <InputGroupAddon align="inline-end">
              <button
                type="button"
                aria-label="Clear search"
                onClick={() => setQuery("")}
                className="text-muted-foreground outline-none hover:text-foreground"
              >
                <XIcon className="size-3.5" />
              </button>
            </InputGroupAddon>
          )}
        </InputGroup>
        <FilterButton query={query} keys={keys} filters={parsed.filters} onQuery={setQuery} tags={tags} />
        <div className="ml-auto">
          <AddMachineDialog />
        </div>
      </div>
      {filtered && list.data && (
        <div className="flex flex-wrap items-center gap-2">
          {applied.map(([key, value]) => (
            <FilterChip key={key} name={key} value={value} query={query} onQuery={setQuery} tags={tags} />
          ))}
          <span className="px-1 text-xs text-muted-foreground">
            {total} {total === 1 ? "machine" : "machines"}
          </span>
        </div>
      )}

      <div>
        {/* Fixed column widths: loading data (or a long name) never moves the columns. */}
        <table className="w-full table-fixed text-sm">
          <colgroup>
            <col />
            <col className="hidden w-[32%] md:table-column lg:w-[28%]" />
            <col className="hidden w-[20%] lg:table-column" />
            <col className="hidden w-[15%] lg:table-column" />
            <col className="w-12" />
          </colgroup>
          <thead className="border-b border-divider text-left text-xs text-muted-foreground">
            <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
              <th>Machine</th>
              <th className="hidden md:table-cell">
                <span className="inline-flex items-center gap-1">
                  <span className="trim-cap">Address</span>
                  <Tooltip>
                    <TooltipTrigger
                      render={
                        <span
                          className="inline-flex text-muted-foreground hover:text-foreground"
                          aria-label="About addresses"
                        />
                      }
                    >
                      <InfoIcon className="size-3.5" />
                    </TooltipTrigger>
                    <TooltipContent className="max-w-64">
                      Open it from any of your machines with Paperboat, by name or IP.
                    </TooltipContent>
                  </Tooltip>
                </span>
              </th>
              <th className="hidden lg:table-cell">Running</th>
              <th className="hidden lg:table-cell">Version</th>
              <th className="w-12" aria-label="Actions" />
            </tr>
          </thead>
          <tbody className="divide-y divide-divider">
            {isPending && (
              <TableRowsSkeleton
                rows={lastRows ?? Math.min(summary.data?.total ?? 12, cursorPageSize)}
                columns={[
                  { bar: "w-28", dot: true },
                  { className: "hidden md:table-cell", bar: "w-40", second: "w-24" },
                  { className: "hidden lg:table-cell", bar: "w-32" },
                  { className: "hidden lg:table-cell", bar: "w-24", second: "w-20" },
                  { empty: true },
                ]}
              />
            )}
            {shown?.map((machine) => (
              <MachineRow
                key={machine.id}
                machine={machine}
                me={me}
                running={runningFailed ? null : running && (running.counts.get(machine.id) ?? (running.truncated ? null : idle))}
              />
            ))}
          </tbody>
        </table>
        {isError && <LoadError what="machines" error={error} retry={() => refetch()} />}
        {list.data && <ListPager page={list.data.page} size={cursorPageSize} total={total} go={pages.go} />}
        {list.data && total === 0 && !filtered && <NoMachines />}
        {list.data && total === 0 && filtered && (
          <p className="py-12 text-center text-sm text-muted-foreground">
            No machines match these filters
          </p>
        )}
      </div>
    </section>
  )
}

const idle: RunningCounts = { terminals: 0, tunnels: 0, previews: 0 }

function MachineRow({
  machine,
  me,
  running,
}: {
  machine: Machine
  me: Me | undefined
  /** Undefined while loading, null when the counts could not be loaded. */
  running: RunningCounts | null | undefined
}) {
  const owner = machineOwner(machine, me)
  const subtitle = owner === "shared" ? "Shared with you" : undefined
  // Right-clicking anywhere on the row opens the same menu as its "…" button.
  return (
    <MachineRowMenus
      machine={machine}
      row={(actions) => (
        <tr className="group relative transition-colors hover:bg-surface data-popup-open:bg-surface [&>td]:h-14 [&>td]:px-3">
          <td>
            <div className="flex h-[1lh] min-w-0 items-center gap-2">
              <SharedName name={`machine-${machine.id}`}>
                <Link
                  href={`/machines/${machine.id}`}
                  className="truncate-x font-medium trim-cap after:absolute after:inset-0"
                >
                  {machine.alias}
                </Link>
              </SharedName>
              <MachineDot machine={machine} />
              {/* Above the row's link overlay, so a tag filters instead of opening the machine. */}
              <TagChips tags={machine.tags} className="relative z-10 hidden min-w-0 sm:inline-flex" />
            </div>
            {subtitle && <div className="truncate text-xs text-muted-foreground">{subtitle}</div>}
          </td>
          <td className="hidden md:table-cell">
            <MachineAddressStack machine={machine} />
          </td>
          <td className="hidden lg:table-cell">
            <Running counts={running} />
          </td>
          <td className="hidden whitespace-nowrap lg:table-cell">
            <span className="inline-flex items-center gap-1.5 font-mono text-code text-muted-foreground">
              {machineVersion(machine)}
              <MachineUpdate machine={machine} />
            </span>
            <div className="text-xs text-muted-foreground">
              {formatPlatform(machine.platform)}{" "}
              <span className="text-muted-foreground/70">{machine.architecture}</span>
            </div>
          </td>
          <td className="text-right">{actions}</td>
        </tr>
      )}
    />
  )
}

const runningKinds = [
  { key: "terminals", short: "term", one: "terminal", many: "terminals" },
  { key: "tunnels", short: "tun", one: "tunnel", many: "tunnels" },
  { key: "previews", short: "prev", one: "preview", many: "previews" },
] as const

/** What the machine is running, abbreviated on one line, every kind always shown; full words on hover. */
function Running({ counts }: { counts: RunningCounts | null | undefined }) {
  if (counts === undefined) return <Skeleton className="h-4 w-32" />
  if (counts === null) return <span className="text-muted-foreground">—</span>
  const full = runningKinds
    .map((kind) => `${counts[kind.key]} ${counts[kind.key] === 1 ? kind.one : kind.many}`)
    .join(", ")
  return (
    <Tooltip>
      <TooltipTrigger
        render={<span className="relative z-10 whitespace-nowrap text-muted-foreground" aria-label={full} />}
      >
        {runningKinds.map((kind, index) => (
          <span key={kind.key}>
            {index > 0 && " · "}
            <span className={counts[kind.key] > 0 ? "text-foreground" : undefined}>{counts[kind.key]}</span>{" "}
            {kind.short}
          </span>
        ))}
      </TooltipTrigger>
      <TooltipContent>{full}</TooltipContent>
    </Tooltip>
  )
}

function NoMachines() {
  const workspace = useWorkspace()
  const team = workspace?.kind === "team"
  return (
    <Empty className="py-16">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <LaptopIcon />
        </EmptyMedia>
        <EmptyTitle>{team ? `No machines in ${workspace.name} yet` : "Add your first machine"}</EmptyTitle>
        {!team && <EmptyDescription>Run one command on a computer to add it.</EmptyDescription>}
      </EmptyHeader>
      <EmptyContent>
        <AddMachineDialog />
      </EmptyContent>
    </Empty>
  )
}

/**
 * The table's page, in the URL (?page=) so a page can be shared and survives a reload. A new
 * search or filter starts again at page 1; a page past the end shows the last one (the server's
 * cursor stops there).
 */
function usePages(query: string) {
  const url = useMachineUrl()
  // The page shown is chosen here and written to the URL; the URL's page applies when the page is
  // opened (a new `url`) and the search is still the URL's. A new search starts at page 1.
  const [chosen, setChosen] = useState<{ query: string; page: number; url: typeof url } | null>(null)
  const wanted = chosen?.url === url ? (chosen.query === query ? chosen.page : 1) : query === url.q ? url.page : 1
  const page = Math.max(1, wanted)
  const go = (next: number) => {
    setChosen({ query, page: next, url })
    const address = new URL(window.location.href)
    if (next > 1) address.searchParams.set("page", String(next))
    else address.searchParams.delete("page")
    window.history.replaceState(null, "", address)
    document.getElementById("machines-table")?.scrollIntoView({ block: "start" })
  }
  return { page, go }
}
