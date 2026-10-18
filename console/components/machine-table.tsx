"use client"

import { InfoIcon, LaptopIcon, MagnifyingGlassIcon, XIcon } from "@phosphor-icons/react"
import Link from "next/link"
import { useDeferredValue } from "react"
import {
  useApiQuery,
  machineOwner,
  meQuery,
  machineSearchQuery,
  useMachinePage,
  useRunning,
  useWorkspace,
  type Machine,
  type Me,
  type RunningCounts,
} from "@/lib/api"
import { formatPlatform } from "@/lib/format"
import { parseQuery, matchesQuery, type FilterKey } from "@/lib/machine-filter"
import { AddMachineDialog } from "@/components/add-machine-dialog"
import { MachineRowMenus } from "@/components/machine-actions"
import { useMachineQuery } from "@/components/machine-query"
import { MachineAddressStack } from "@/components/machine-address"
import { FilterButton, FilterChip } from "@/components/machine-filters"
import { MachineUpdate } from "@/components/machine-update"
import { MachineDot, machineVersion } from "@/components/machine-status"
import { Button } from "@/components/ui/button"
import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group"
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { Skeleton } from "@/components/ui/skeleton"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"

export function MachineTable() {
  const { data: page, isPending, isError, refetch } = useMachinePage()
  const { data: me } = useApiQuery(meQuery)
  const { data: running, isError: runningFailed } = useRunning()
  const workspace = useWorkspace()
  const [query, setQuery] = useMachineQuery()
  const parsed = parseQuery(useDeferredValue(query))
  // The first page holds up to 200 machines; past that, typing searches the whole workspace.
  const partial = Boolean(page && page.total > page.items.length)
  const search = useApiQuery({
    ...machineSearchQuery(workspace?.id ?? "", parsed.text),
    enabled: partial && Boolean(workspace && parsed.text),
  })
  const machines = partial && parsed.text ? search.data?.items : page?.items
  const keys: FilterKey[] = workspace?.kind === "personal" ? ["status", "os", "owner"] : ["status", "os"]
  const applied = (Object.entries(parsed.filters) as [FilterKey, string][]).filter(([key]) => keys.includes(key))
  const visible = machines?.filter((machine) =>
    matchesQuery(machine, { text: parsed.text, filters: Object.fromEntries(applied) }, me),
  )
  const filtered = Boolean(parsed.text) || applied.length > 0

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
        <FilterButton query={query} keys={keys} filters={parsed.filters} onQuery={setQuery} />
        <div className="ml-auto">
          <AddMachineDialog />
        </div>
      </div>
      {filtered && machines && (
        <div className="flex flex-wrap items-center gap-2">
          {applied.map(([key, value]) => (
            <FilterChip key={key} name={key} value={value} query={query} onQuery={setQuery} />
          ))}
          <span className="px-1 text-xs text-muted-foreground">
            {visible?.length} of {machines.length} {machines.length === 1 ? "machine" : "machines"}
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
                      Reachable from any machine signed in with Paperboat, by address or by IP.
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
            {(isPending || (partial && Boolean(parsed.text) && search.isPending)) && <LoadingRows />}
            {visible?.map((machine) => (
              <MachineRow
                key={machine.id}
                machine={machine}
                me={me}
                running={runningFailed ? null : running && (running.get(machine.id) ?? idle)}
              />
            ))}
          </tbody>
        </table>
        {isError && (
          <div className="flex items-center justify-center gap-3 py-12 text-sm text-muted-foreground">
            Couldn’t load machines.
            <Button variant="outline" size="sm" onClick={() => refetch()}>
              Retry
            </Button>
          </div>
        )}
        {partial && !parsed.text && page && (
          <p className="py-4 text-center text-xs text-muted-foreground">
            Showing {page.items.length} of {page.total} machines. Search to find the others.
          </p>
        )}
        {machines?.length === 0 && !filtered && <NoMachines />}
        {machines && visible?.length === 0 && (machines.length > 0 || filtered) && (
          <p className="py-12 text-center text-sm text-muted-foreground">
            {filtered ? "No machines match these filters" : "No machines"}
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
  const subtitle = machine.description || (owner === "shared" ? "Shared with you" : undefined)
  // Right-clicking anywhere on the row opens the same menu as its "…" button.
  return (
    <MachineRowMenus
      machine={machine}
      row={(actions) => (
        <tr className="group relative transition-colors hover:bg-surface data-popup-open:bg-surface [&>td]:h-14 [&>td]:px-3">
          <td>
            <div className="flex h-[1lh] min-w-0 items-center gap-2">
              <Link
                href={`/machines/${machine.id}`}
                className="truncate-x font-medium trim-cap after:absolute after:inset-0"
              >
                {machine.alias}
              </Link>
              <MachineDot machine={machine} />
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

function LoadingRows() {
  return Array.from({ length: 3 }, (_, index) => (
    <tr key={index} className="[&>td]:h-14 [&>td]:px-3">
      <td>
        <Skeleton className="h-4 w-32" />
      </td>
      <td className="hidden md:table-cell">
        <Skeleton className="h-4 w-24" />
      </td>
      <td className="hidden lg:table-cell">
        <Skeleton className="h-4 w-20" />
      </td>
      <td className="hidden lg:table-cell">
        <Skeleton className="h-4 w-14" />
      </td>
      <td />
    </tr>
  ))
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
        <EmptyDescription>
          {team
            ? "Add one of your machines so the team can reach it."
            : "Run one command on a computer to reach it from anywhere."}
        </EmptyDescription>
      </EmptyHeader>
      <EmptyContent>
        <AddMachineDialog />
      </EmptyContent>
    </Empty>
  )
}
