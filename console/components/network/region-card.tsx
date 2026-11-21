"use client"

import { DotsThreeIcon, PencilSimpleIcon, PlusIcon, TrashIcon } from "@phosphor-icons/react"
import { regionState, type Node, type Region, type RegionState } from "@/lib/network"
import { MachineRow } from "@/components/network/machine-row"
import { StatusDot } from "@/components/machine-status"
import { Button } from "@/components/ui/button"
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuTrigger } from "@/components/ui/dropdown-menu"
import { Skeleton } from "@/components/ui/skeleton"
import type { Role } from "@/components/network/add-machine-to-region-dialog"

const stateLabel: Record<RegionState, string> = {
  ready: "Ready",
  degraded: "Degraded",
  draining: "Draining",
  not_ready: "Not ready",
  empty: "Empty",
}

const count = (machines: number) => (machines === 1 ? "1 machine" : `${machines} machines`)

/** One of your own regions: its name (slug muted), status and machine count, the machines inside it, and the ways to grow it. */
export function RegionCard({
  region,
  nodes,
  onAdd,
  onRename,
  onRemove,
}: {
  region: Region
  nodes: Node[]
  onAdd: (role: Role) => void
  onRename: () => void
  onRemove: () => void
}) {
  const state = regionState(region, nodes)
  // Machines still pairing are not counted by the server yet, but their rows are on the page.
  const machines = Math.max(region.machines, nodes.length)
  return (
    <section className="flex min-w-0 flex-col gap-3 surface rounded-xl p-4" data-region={region.id}>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div className="flex min-w-0 flex-1 basis-48 flex-col gap-0.5">
          <h3 className="truncate text-sm font-medium">{region.name}</h3>
          <p className="truncate font-mono text-xs text-muted-foreground">{region.id}</p>
        </div>
        <p className="flex items-center gap-2 text-sm text-muted-foreground">
          <StatusDot online={state === "ready"} />
          <span data-region-status>{stateLabel[state]}</span>
          <span aria-hidden>·</span>
          <span>{count(machines)}</span>
        </p>
        <div className="flex items-center gap-2">
          <Button variant="outline" onClick={() => onAdd("relay")}>
            <PlusIcon data-icon="inline-start" />
            Add relay
          </Button>
          <Button variant="outline" onClick={() => onAdd("edge")}>
            <PlusIcon data-icon="inline-start" />
            Add edge
          </Button>
          <DropdownMenu>
            <DropdownMenuTrigger
              render={
                <Button
                  variant="ghost"
                  size="icon-sm"
                  aria-label={`Actions for ${region.name}`}
                  className="text-muted-foreground hover:text-foreground"
                />
              }
            >
              <DotsThreeIcon weight="bold" className="size-4" />
            </DropdownMenuTrigger>
            <DropdownMenuContent align="end" className="w-48">
              <DropdownMenuItem onClick={onRename}>
                <PencilSimpleIcon /> Rename
              </DropdownMenuItem>
              <DropdownMenuItem variant="destructive" onClick={onRemove}>
                <TrashIcon /> Remove region
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>
      {nodes.length === 0 ? (
        region.machines === 0 && (
          <p className="border-t border-divider pt-3 text-sm text-muted-foreground">
            No machines yet. Add a relay or an edge to bring this region online.
          </p>
        )
      ) : (
        <table className="w-full table-fixed text-sm">
          <colgroup>
            <col className="w-[34%] sm:w-[26%]" />
            <col className="hidden w-[16%] md:table-column" />
            <col className="hidden w-[14%] sm:table-column" />
            <col />
            <col className="hidden w-[14%] lg:table-column" />
            <col className="w-12" />
          </colgroup>
          <thead className="border-b border-divider text-left text-xs text-muted-foreground">
            <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
              <th>Machine</th>
              <th className="hidden md:table-cell">Role</th>
              <th className="hidden sm:table-cell">Version</th>
              <th>Status</th>
              <th className="hidden lg:table-cell">Last seen</th>
              <th className="w-12" aria-label="Actions" />
            </tr>
          </thead>
          <tbody className="divide-y divide-divider">
            {nodes.map((node) => (
              <MachineRow key={node.id} node={node} />
            ))}
          </tbody>
        </table>
      )}
    </section>
  )
}

/** The card while regions and machines load: the buttons are real, only the values are placeholders. */
export function RegionCardSkeleton() {
  return (
    <section className="flex flex-col gap-3 surface rounded-xl p-4">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-2">
        <div className="flex min-w-0 flex-1 basis-48 flex-col gap-0.5">
          <Skeleton className="my-0.5 h-4 w-32" />
          <Skeleton className="my-0.5 h-4 w-20" />
        </div>
        <Skeleton className="my-0.5 h-4 w-32" />
        <div className="flex items-center gap-2">
          <Button variant="outline" disabled>
            <PlusIcon data-icon="inline-start" />
            Add relay
          </Button>
          <Button variant="outline" disabled>
            <PlusIcon data-icon="inline-start" />
            Add edge
          </Button>
        </div>
      </div>
    </section>
  )
}
