"use client"

import { CaretUpDownIcon } from "@phosphor-icons/react"
import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query"
import { useEffect, useState } from "react"
import { toast } from "sonner"
import { edgesQuery, useApiQuery, useWorkspace } from "@/lib/api"
import {
  edgePairQuery,
  liveOf,
  poolServersQuery,
  regionsQuery,
  saveEdgePair,
  scopeOf,
  switchEdgesStoppingLive,
  type EdgePair,
  type LiveItem,
  type PairKind,
  type PairRegion,
} from "@/lib/network"
import { useShellState } from "@/lib/use-shell-state"
import { pickerOption } from "@/components/crumb-picker"
import { StatusDot } from "@/components/machine-status"
import { latency, RegionLine, RegionTable } from "@/components/network/region-table"
import { StopAndSwitch, SwitchedEdges } from "@/components/network/switch-edges"
import { SectionHeading } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Command, CommandEmpty, CommandGroup, CommandItem, CommandList } from "@/components/ui/command"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldLabel } from "@/components/ui/field"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import { LoadError } from "@/components/load-error"
import { ErrorMessage } from "@/components/error-reference"

/**
 * The workspace's pair of regions (GET /v1/edges/pair): where its tunnels and previews are served.
 * Machines pick between the two by what they measure, so the labels are roles, not a promise of
 * which one carries traffic. Owners and admins change the pair.
 */
export function EdgesSection({ workspace, manage }: { workspace: string; manage: boolean }) {
  const pair = useApiQuery({ ...edgePairQuery(workspace), enabled: Boolean(workspace) })
  const scope = scopeOf(useWorkspace())
  const own = useApiQuery(regionsQuery(scope))
  // Your own regions are named by their display name; the slug stays muted beside it.
  const label = (region: PairRegion) => {
    const named = pair.data?.kind === "self_hosted" ? own.data?.find((item) => item.id === region.id) : undefined
    return named ? (
      <>
        {named.name}
        {named.name !== named.id && <span className="ml-2 font-mono text-xs text-muted-foreground">{named.id}</span>}
      </>
    ) : (
      region.name
    )
  }
  const [changing, setChanging] = useShellState("network/network-page.tsx:changing", false)
  const regions: { role: string; region: PairRegion }[] = []
  if (pair.data) regions.push({ role: "Primary region", region: pair.data.primary })
  if (pair.data?.fallback) regions.push({ role: "Backup region", region: pair.data.fallback })
  return (
    <section className="flex flex-col gap-3">
      <div className="flex flex-col gap-1">
        <SectionHeading
          title="Edges"
          action={
            manage &&
            pair.isSuccess && (
              <Button variant="secondary" size="lg" onClick={() => setChanging(true)}>
                Change
              </Button>
            )
          }
        />
        <p className="text-sm text-muted-foreground">Your tunnels and previews are served from these two regions.</p>
      </div>
      <RegionTable roles showLatency loading={pair.isPending} rows={2}>
        {regions.map(({ role, region }) => (
          <RegionLine
            key={role}
            role={role}
            name={label(region)}
            status={region.status}
            rtt={region.best_rtt_ms}
            showLatency
          />
        ))}
      </RegionTable>
      {pair.isSuccess && !pair.data && <p className="py-3 text-sm text-muted-foreground">No regions assigned yet.</p>}
      {pair.isError && <LoadError what="regions" error={pair.error} retry={() => pair.refetch()} />}
      {changing && pair.data !== undefined && (
        <ChangeEdgesDialog workspace={workspace} current={pair.data} onClose={() => setChanging(false)} />
      )}
    </section>
  )
}

/** A region to choose in Change regions: one of Paperboat's, or one of your own (grouping your machines). */
type RegionOption = {
  /** `kind:id`: a Paperboat region and one of your own can share an id. */
  key: string
  kind: PairKind
  name: string
  /** For your own regions, the slug when the name differs from it (shown muted). */
  slug?: string
  /** For your own regions, how many of your machines are in it. */
  detail?: string
  ok: boolean
  rtt?: number
  /** "Primary" or "Backup" when it is one now. */
  role?: string
}

/** One region as the table shows it: name (and its servers), current role, status dot, latency. */
function RegionLabel({ option }: { option: RegionOption }) {
  return (
    <span className="flex min-w-0 flex-1 items-center gap-3 text-left">
      <span className="min-w-0 flex-1 truncate">
        {option.name}
        {option.slug && <span className="font-mono text-xs text-muted-foreground"> {option.slug}</span>}
        {option.detail && <span className="text-muted-foreground"> · {option.detail}</span>}
      </span>
      {option.role && <span className="text-xs text-muted-foreground">{option.role}</span>}
      <StatusDot online={option.ok} />
      <span className="w-12 text-right font-mono text-code text-muted-foreground">{latency(option.rtt)}</span>
    </span>
  )
}

/** The Primary or Backup region field: the console's picker (popover + list), like choosing a machine. */
function RegionChoice({
  id,
  options,
  value,
  onChange,
}: {
  id: string
  options: RegionOption[]
  value: string
  onChange: (key: string) => void
}) {
  const [open, setOpen] = useShellState("network/network-page.tsx:open", false)
  const chosen = options.find((option) => option.key === value)
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger
        render={
          <Button
            id={id}
            type="button"
            variant="outline"
            className="h-auto min-h-12 w-full justify-between py-1.5 font-normal"
          />
        }
      >
        {chosen ? <RegionLabel option={chosen} /> : <span className="text-muted-foreground">Choose a region</span>}
        <CaretUpDownIcon className="shrink-0 text-muted-foreground" />
      </PopoverTrigger>
      <PopoverContent align="start" className="w-(--anchor-width) gap-0 p-0">
        <Command>
          <CommandList>
            <CommandEmpty>No regions</CommandEmpty>
            <CommandGroup>
              {options.map((option) => (
                <CommandItem
                  key={option.key}
                  value={option.key}
                  className={pickerOption}
                  onSelect={() => {
                    onChange(option.key)
                    setOpen(false)
                  }}
                >
                  <RegionLabel option={option} />
                </CommandItem>
              ))}
            </CommandGroup>
          </CommandList>
        </Command>
      </PopoverContent>
    </Popover>
  )
}

/**
 * Picks the pair from Paperboat's regions and your own (the regions you named for your tunnel servers): both
 * of one kind (the server refuses a mix). Switching kinds needs running previews and tunnels stopped first (409).
 */
function ChangeEdgesDialog({
  workspace,
  current,
  onClose,
}: {
  workspace: string
  current: EdgePair | null
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  const scope = scopeOf(useWorkspace())
  const managed = useApiQuery(edgesQuery)
  const own = useInfiniteQuery(poolServersQuery("tunnel", scope, ""))
  const named = useApiQuery(regionsQuery(scope))
  // Your own regions are counted from all your servers, so read every page.
  useEffect(() => {
    if (own.hasNextPage && !own.isFetchingNextPage) void own.fetchNextPage()
  }, [own])
  // Latency is measured for the pair's regions only.
  const inPair = (kind: PairKind, id: string) =>
    current?.kind !== kind
      ? undefined
      : current.primary.id === id
        ? { region: current.primary, role: "Primary" }
        : current.fallback?.id === id
          ? { region: current.fallback, role: "Backup" }
          : undefined
  const ownRegions = new Map<string, { servers: number; ready: boolean }>()
  for (const server of own.data?.pages.flatMap((page) => page.items) ?? []) {
    const seen = ownRegions.get(server.region)
    ownRegions.set(server.region, { servers: (seen?.servers ?? 0) + 1, ready: Boolean(seen?.ready) || server.ready })
  }
  const options: RegionOption[] = [
    ...(managed.data ?? []).map((region) => {
      const known = inPair("managed", region.id)
      return {
        key: `managed:${region.id}`,
        kind: "managed" as const,
        name: region.name,
        ok: region.status === "ready",
        rtt: known?.region.best_rtt_ms,
        role: known?.role,
      }
    }),
    ...[...ownRegions].map(([id, region]) => {
      const known = inPair("self_hosted", id)
      const name = named.data?.find((item) => item.id === id)?.name ?? id
      return {
        key: `self_hosted:${id}`,
        kind: "self_hosted" as const,
        name,
        slug: name === id ? undefined : id,
        detail: region.servers === 1 ? "1 machine" : `${region.servers} machines`,
        ok: region.ready,
        rtt: known?.region.best_rtt_ms,
        role: known?.role,
      }
    }),
  ]
  const keyOf = (region?: PairRegion) => (region && current ? `${current.kind}:${region.id}` : "")
  const [primary, setPrimary] = useShellState("network/network-page.tsx:primary", keyOf(current?.primary))
  const [fallback, setFallback] = useShellState("network/network-page.tsx:fallback", keyOf(current?.fallback))
  const kindOf = (key: string) => options.find((option) => option.key === key)?.kind
  const regionOf = (key: string) => key.slice(key.indexOf(":") + 1)
  const choice = () => ({
    kind: kindOf(primary) ?? "managed",
    primaryRegion: regionOf(primary),
    fallbackRegion: regionOf(fallback),
  })
  const problem =
    !primary || !fallback
      ? undefined
      : primary === fallback
        ? "Choose two different regions."
        : kindOf(primary) !== kindOf(fallback)
          ? "Choose two Paperboat regions or two of your own."
          : undefined
  const [switched, setSwitched] = useState<LiveItem[]>()
  // Changing kind strands domains and what runs, so it ends on the cutover view; a reorder just closes.
  const changed = (pair: EdgePair, stopped: LiveItem[]) => {
    queryClient.setQueryData(edgePairQuery(workspace).queryKey, pair)
    queryClient.invalidateQueries({ queryKey: ["previews"] })
    queryClient.invalidateQueries({ queryKey: ["tunnels"] })
    if (kindOf(primary) === current?.kind) {
      toast.success("Regions changed")
      onClose()
    } else setSwitched(stopped)
  }
  const save = useMutation({
    mutationFn: () => saveEdgePair(workspace, choice()),
    onSuccess: (pair) => changed(pair, []),
  })
  const stopAndSwitch = useMutation({
    mutationFn: () => switchEdgesStoppingLive(workspace, choice()),
    onSuccess: ({ pair, stopped }) => changed(pair, stopped),
  })
  // The latest refusal names what is still running; any other error is shown as it is.
  const live = liveOf(stopAndSwitch.error) ?? liveOf(save.error)
  const failed = stopAndSwitch.error ?? save.error
  const failure = failed && !liveOf(failed) && <ErrorMessage error={failed} />
  const busy = save.isPending || stopAndSwitch.isPending
  const pick = (set: (id: string) => void) => (id: string) => {
    save.reset()
    stopAndSwitch.reset()
    set(id)
  }
  if (switched)
    return (
      <Dialog open onOpenChange={(open) => !open && onClose()}>
        <DialogContent className="gap-4 sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>Regions changed</DialogTitle>
          </DialogHeader>
          <SwitchedEdges
            workspace={workspace}
            stopped={switched}
            toOwnServers={kindOf(primary) === "self_hosted"}
            onClose={onClose}
          />
        </DialogContent>
      </Dialog>
    )
  return (
    <Dialog open onOpenChange={(open) => !open && !busy && onClose()}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Change regions</DialogTitle>
        </DialogHeader>
        <Field className="gap-1.5">
          <FieldLabel htmlFor="edge-primary">Primary region</FieldLabel>
          <RegionChoice id="edge-primary" options={options} value={primary} onChange={pick(setPrimary)} />
        </Field>
        <Field className="gap-1.5">
          <FieldLabel htmlFor="edge-fallback">Backup region</FieldLabel>
          <RegionChoice id="edge-fallback" options={options} value={fallback} onChange={pick(setFallback)} />
        </Field>
        {problem && <p className="text-sm text-destructive">{problem}</p>}
        {failure && <p className="text-sm text-destructive">{failure}</p>}
        {live && live.items.length > 0 ? (
          <StopAndSwitch
            items={live.items}
            count={live.count}
            pending={busy}
            onConfirm={() => stopAndSwitch.mutate()}
          />
        ) : (
          live && <p className="text-sm text-destructive">Stop running previews and tunnels first.</p>
        )}
        <DialogFooter>
          <Button variant="ghost" size="lg" disabled={busy} onClick={onClose}>
            Cancel
          </Button>
          <Button
            size="lg"
            disabled={!kindOf(primary) || !kindOf(fallback) || Boolean(problem) || busy || Boolean(live?.items.length)}
            onClick={() => {
              stopAndSwitch.reset()
              save.mutate()
            }}
          >
            {save.isPending ? "Saving…" : "Save"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
