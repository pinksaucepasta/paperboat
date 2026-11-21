"use client"

import { GlobeHemisphereWestIcon, PlusIcon } from "@phosphor-icons/react"
import { useDeferredValue } from "react"
import { useApiQuery, type Workspace } from "@/lib/api"
import { cursorPageSize } from "@/lib/cursor-pages"
import { groupNodes, installationsQuery, regionsQuery, scopeOf, updating, type Region } from "@/lib/network"
import { usePageParam } from "@/lib/use-page-param"
import { useShellState } from "@/lib/use-shell-state"
import { AddMachineToRegionDialog, type Role } from "@/components/network/add-machine-to-region-dialog"
import { RegionCard, RegionCardSkeleton } from "@/components/network/region-card"
import { RegionNameDialog, RemoveRegionAction } from "@/components/network/region-dialogs"
import { ListPager } from "@/components/list-pager"
import { SearchField } from "@/components/list-toolbar"
import { LoadError } from "@/components/load-error"
import { SectionHeading } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"

type Dialog =
  | { kind: "create" }
  | { kind: "rename"; region: Region }
  | { kind: "remove"; region: Region }
  | { kind: "add"; region: Region; role: Role }

/**
 * Your own regions: each one a card with the machines inside it (grouped by node), a way to add a relay or an edge,
 * and Rename / Remove. Regions come from the regions list (empty ones too); machines come from the paged, searchable
 * installations list. A machine whose region the list does not have yet (still pairing) gets its region shown too.
 */
export function RegionsSection({ workspace }: { workspace: Workspace | undefined }) {
  const scope = scopeOf(workspace)
  const team = workspace?.kind === "team"
  const [search, setSearch] = useShellState("network/regions-section.tsx:search", "")
  const q = useDeferredValue(search.trim())
  const pages = usePageParam(q)
  const [dialog, setDialog] = useShellState<Dialog | undefined>("network/regions-section.tsx:dialog")
  const regions = useApiQuery({ ...regionsQuery(scope), enabled: Boolean(workspace) })
  const installations = useApiQuery({
    ...installationsQuery(scope, q, pages.page),
    enabled: Boolean(workspace),
    // Live progress while a node updates.
    refetchInterval: (query) => (query.state.data?.items.some(updating) ? 5000 : 10_000),
  })
  const nodes = groupNodes(installations.data?.items ?? [])
  const known = new Set((regions.data ?? []).map((region) => region.id))
  const pending: Region[] = [...new Set(nodes.map((node) => node.region))]
    .filter((id) => !known.has(id))
    .map((id) => ({ id, name: id, machines: 0, ready: 0 }))
  const all = [...(regions.data ?? []), ...pending]
  const needle = q.toLowerCase()
  const shown = q
    ? all.filter(
        (region) =>
          nodes.some((node) => node.region === region.id) ||
          region.name.toLowerCase().includes(needle) ||
          region.id.includes(needle),
      )
    : all
  const loading = regions.isPending || installations.isPending
  const none = regions.isSuccess && installations.isSuccess && all.length === 0 && !q

  return (
    <section className="flex flex-col gap-3">
      <SectionHeading
        title={team ? `${workspace?.name ?? "Team"} regions` : "Your regions"}
        action={
          <div className="flex items-center gap-2">
            {(q || (installations.data?.total ?? 0) > cursorPageSize) && (
              <SearchField value={search} onChange={setSearch} placeholder="Search machines" />
            )}
            <Button size="lg" onClick={() => setDialog({ kind: "create" })}>
              <PlusIcon data-icon="inline-start" />
              New region
            </Button>
          </div>
        }
      />
      {loading && !regions.isError && !installations.isError && <RegionCardSkeleton />}
      {regions.isError && <LoadError what="your regions" error={regions.error} retry={() => regions.refetch()} />}
      {installations.isError && !regions.isError && (
        <LoadError what="your machines" error={installations.error} retry={() => installations.refetch()} />
      )}
      {none && (
        <Empty className="py-16">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <GlobeHemisphereWestIcon />
            </EmptyMedia>
            <EmptyTitle>Run your own servers in a region you name</EmptyTitle>
            <EmptyDescription>Create a region, then add a relay or an edge to it.</EmptyDescription>
          </EmptyHeader>
          <EmptyContent>
            <Button size="lg" onClick={() => setDialog({ kind: "create" })}>
              <PlusIcon data-icon="inline-start" />
              New region
            </Button>
          </EmptyContent>
        </Empty>
      )}
      {shown.map((region) => (
        <RegionCard
          key={region.id}
          region={region}
          nodes={nodes.filter((node) => node.region === region.id)}
          onAdd={(role) => setDialog({ kind: "add", region, role })}
          onRename={() => setDialog({ kind: "rename", region })}
          onRemove={() => setDialog({ kind: "remove", region })}
        />
      ))}
      {regions.isSuccess && installations.isSuccess && shown.length === 0 && q && (
        <p className="py-12 text-center text-sm text-muted-foreground">No machines match “{q}”</p>
      )}
      {installations.data && regions.isSuccess && (
        <ListPager page={pages.page} size={cursorPageSize} total={installations.data.total} go={pages.go} />
      )}

      {dialog?.kind === "create" && <RegionNameDialog scope={scope} onClose={() => setDialog(undefined)} />}
      {dialog?.kind === "rename" && (
        <RegionNameDialog scope={scope} region={dialog.region} onClose={() => setDialog(undefined)} />
      )}
      {dialog?.kind === "add" && (
        <AddMachineToRegionDialog
          scope={scope}
          region={dialog.region}
          role={dialog.role}
          onClose={() => setDialog(undefined)}
        />
      )}
      {dialog?.kind === "remove" && (
        <RemoveRegionAction
          scope={scope}
          region={dialog.region}
          open
          onOpenChange={(open) => !open && setDialog(undefined)}
        />
      )}
    </section>
  )
}
