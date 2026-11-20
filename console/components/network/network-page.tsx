"use client"

import {
  ArrowClockwiseIcon,
  ArrowsLeftRightIcon,
  CaretUpDownIcon,
  CheckCircleIcon,
  CopyIcon,
  PauseIcon,
  PlayIcon,
  PlusIcon,
  ShieldCheckIcon,
  TrashIcon,
} from "@phosphor-icons/react"
import { useInfiniteQuery, useMutation, useQueryClient, type QueryClient } from "@tanstack/react-query"
import { useDeferredValue, useEffect, useState } from "react"
import { toast } from "sonner"
import { clientConfigQuery, edgesQuery, relaysQuery, useApiQuery, useWorkspace } from "@/lib/api"
import { copyWithToast } from "@/lib/clipboard"
import { cursorPageSize } from "@/lib/cursor-pages"
import {
  drainInstallation,
  edgePairQuery,
  groupNodes,
  installCommand,
  installationsQuery,
  pairNode,
  poolQuery,
  poolServersQuery,
  removeInstallation,
  liveOf,
  requestUpdate,
  resumeInstallation,
  saveEdgePair,
  savePool,
  scopeOf,
  switchEdgesStoppingLive,
  updating,
  verifyIngress,
  type EdgePair,
  type LiveItem,
  type PairKind,
  type PairRegion,
  type Node,
} from "@/lib/network"
import { useNow } from "@/lib/use-now"
import { usePageParam } from "@/lib/use-page-param"
import { CommandBlock } from "@/components/add-machine-dialog"
import { DomainList } from "@/components/domains/domain-list"
import { PortNames } from "@/components/network/port-names"
import { StopAndSwitch, SwitchedEdges } from "@/components/network/switch-edges"
import { pickerOption } from "@/components/crumb-picker"
import { ListPager } from "@/components/list-pager"
import { ConfirmAction, SearchField } from "@/components/list-toolbar"
import { StatusDot } from "@/components/machine-status"
import { RowMenus, type Entry } from "@/components/row-menus"
import { Time } from "@/components/time"
import { Section, SectionHeading } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Command, CommandEmpty, CommandGroup, CommandItem, CommandList } from "@/components/ui/command"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import { Switch } from "@/components/ui/switch"
import { Skeleton } from "@/components/ui/skeleton"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { TableRowsSkeleton } from "@/components/skeletons"
import { LoadError } from "@/components/load-error"
import { ErrorMessage, errorToast } from "@/components/error-reference"
import { useShellState } from "@/lib/use-shell-state"

/*
 * Paperboat's regions and your own servers, and which regions your traffic uses. Users see regions, never
 * Paperboat's nodes; your own servers keep their names and addresses, grouped under the region you named
 * when pairing them (servers of one region back each other up). Machines connect directly whenever they
 * can; a relay carries what can't go directly (it never sees inside the encryption); a tunnel server is
 * where your tunnels and previews meet the internet (browser HTTPS ends there).
 */

/**
 * After a server is paired or removed, or the relay pool changes: your servers and pools, and the
 * Relays and Edges tables (Paperboat's relay regions, the workspace's pair of regions) that follow from them.
 */
const refreshNetwork = (queryClient: QueryClient) =>
  Promise.all([["selfhost"], ["relays"], ["edges"]].map((queryKey) => queryClient.invalidateQueries({ queryKey })))

const runs = (node: Node) => {
  const kinds = new Set(node.installations.map((installation) => installation.capability))
  return kinds.size === 2 ? "Relay and tunnel server" : kinds.has("tunnel") ? "Tunnel server" : "Relay"
}

const updateSteps: Record<string, string> = {
  requested: "waiting for the server to check in",
  downloading: "downloading",
  verifying: "verifying the signed release",
  swapping: "keeping the current version",
  checking: "installing and checking health",
}

function nodeState(node: Node, now: number): { ok: boolean; label: string; detail?: string } {
  if (node.installations.some((installation) => installation.enrollment_state === "pending"))
    return { ok: false, label: "Finishing pairing" }
  const running = node.installations.find(updating)?.update
  if (running)
    return { ok: false, label: `Updating to ${running.target_version}: ${updateSteps[running.state] ?? running.state}` }
  // A failed update stays shown for a day: the reason and its support reference are in the tooltip.
  const failed = node.installations
    .map((installation) => installation.update)
    .find(
      (update) =>
        update &&
        (update.state === "failed" || update.state === "rolled_back") &&
        now - Date.parse(update.updated_at) < 86_400_000,
    )
  if (failed)
    return {
      ok: false,
      label: failed.state === "rolled_back" ? "Update rolled back" : "Update failed",
      detail: `${failed.detail ?? ""}${failed.support_reference ? ` Support reference ${failed.support_reference}.` : ""}`,
    }
  const required = node.installations.find((installation) => installation.update_required)
  if (required) return { ok: false, label: "Update required", detail: required.update_required }
  const notReady = node.installations.filter((installation) => !installation.ready)
  if (!notReady.length) return { ok: true, label: "Ready" }
  if (notReady.length === node.installations.length) return { ok: false, label: "Not ready" }
  return { ok: false, label: `${notReady[0].capability === "tunnel" ? "Tunnel server" : "Relay"} not ready` }
}

export function NetworkPage() {
  const workspace = useWorkspace()
  const scope = scopeOf(workspace)
  const team = workspace?.kind === "team"
  const [search, setSearch] = useShellState("network/network-page.tsx:search", "")
  const q = useDeferredValue(search.trim())
  const pages = usePageParam(q)
  const installations = useApiQuery({
    ...installationsQuery(scope, q, pages.page),
    enabled: Boolean(workspace),
    // Live progress while a node updates.
    refetchInterval: (query) => (query.state.data?.items.some(updating) ? 5000 : 10_000),
  })
  const [adding, setAdding] = useShellState<"relay" | "tunnel" | undefined>("network/network-page.tsx:adding")
  const nodes = groupNodes(installations.data?.items ?? [])
  const none = installations.isSuccess && installations.data.total === 0 && !q

  return (
    <div className="flex flex-col gap-8">
      <RelaysSection scope={scope} />
      <EdgesSection
        workspace={workspace?.id ?? ""}
        manage={workspace?.kind === "personal" || workspace?.role === "owner" || workspace?.role === "admin"}
      />

      <DomainList />

      <PortNames />

      <Section
        title={team ? `${workspace?.name ?? "Team"} servers` : "Your servers"}
        action={
          <div className="flex items-center gap-2">
            {(q || (installations.data?.total ?? 0) > cursorPageSize) && (
              <SearchField value={search} onChange={setSearch} placeholder="Search servers" />
            )}
            <Button size="lg" onClick={() => setAdding("relay")}>
              <PlusIcon data-icon="inline-start" />
              Add server
            </Button>
          </div>
        }
      >
        {none ? (
          <p className="border-b border-divider py-3 text-sm text-muted-foreground">
            None. Add a server to run your own relay or edge.
          </p>
        ) : (
          <div>
            <table className="w-full table-fixed text-sm">
              <colgroup>
                <col className="w-[22%]" />
                <col className="hidden w-[12%] md:table-column" />
                <col className="hidden w-[14%] md:table-column" />
                <col className="hidden sm:table-column" />
                <col className="w-[18%]" />
                <col className="hidden w-[12%] lg:table-column" />
                <col className="w-12" />
              </colgroup>
              <thead className="border-b border-divider text-left text-xs text-muted-foreground">
                <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
                  <th>Server</th>
                  <th className="hidden md:table-cell">Region</th>
                  <th className="hidden md:table-cell">Role</th>
                  <th className="hidden sm:table-cell">Address</th>
                  <th>Status</th>
                  <th className="hidden lg:table-cell">Last seen</th>
                  <th className="w-12" aria-label="Actions" />
                </tr>
              </thead>
              <tbody className="divide-y divide-divider border-b border-divider">
                {installations.isPending ? (
                  <TableRowsSkeleton
                    rows={1}
                    columns={[
                      { bar: "w-28" },
                      { className: "hidden md:table-cell", bar: "w-16" },
                      { className: "hidden md:table-cell", bar: "w-24" },
                      { className: "hidden sm:table-cell", bar: "w-36" },
                      { bar: "w-16" },
                      { className: "hidden lg:table-cell", bar: "w-16" },
                      { empty: true },
                    ]}
                  />
                ) : (
                  nodes.map((node) => <NodeRow key={node.id} node={node} />)
                )}
              </tbody>
            </table>
            {installations.data && (
              <ListPager page={pages.page} size={cursorPageSize} total={installations.data.total} go={pages.go} />
            )}
            {installations.isError && (
              <LoadError what="your servers" error={installations.error} retry={() => installations.refetch()} />
            )}
            {installations.isSuccess && nodes.length === 0 && q && (
              <p className="py-12 text-center text-sm text-muted-foreground">No servers match “{q}”</p>
            )}
          </div>
        )}
      </Section>

      {adding && <AddServerDialog initial={adding} onClose={() => setAdding(undefined)} />}
    </div>
  )
}

function NodeRow({ node }: { node: Node }) {
  const queryClient = useQueryClient()
  const [removing, setRemoving] = useState(false)
  const [draining, setDraining] = useState<"drain" | "move">()
  const state = nodeState(node, useNow())
  // A server being drained reports not ready (the list has no separate flag); Resume puts it back.
  const stopped = node.installations.every(
    (installation) => !installation.ready && installation.enrollment_state === "enrolled",
  )
  const tunnel = node.installations.find((installation) => installation.capability === "tunnel")
  const available = node.installations.find((installation) => installation.update_available)?.update_available
  // The newest heartbeat of the node's components.
  const seen = node.installations
    .map((installation) => installation.last_heartbeat_at)
    .filter(Boolean)
    .sort()
    .at(-1)
  const refresh = () => refreshNetwork(queryClient)
  const update = useMutation({
    mutationFn: () => requestUpdate(node.installations[0]),
    onSuccess: async () => {
      await refresh()
      toast.success(`${node.name} updates at its next check-in`)
    },
    onError: (error) => errorToast("Update could not be requested", error),
  })
  const verify = useMutation({
    mutationFn: () => verifyIngress(tunnel!),
    onSuccess: async () => {
      await refresh()
      toast.success(`${node.name} answers from the internet`)
    },
    onError: (error) => errorToast("Public access check failed", error),
  })
  const resume = useMutation({
    mutationFn: () => resumeInstallation(node.installations[0]),
    onSuccess: async () => {
      await refresh()
      toast.success(`${node.name} takes connections again`)
    },
    onError: (error) => errorToast("Server could not be resumed", error),
  })
  const entries: Entry[] = [
    { kind: "item", label: "Copy address", icon: CopyIcon, onSelect: () => void copyWithToast(node.host, "Address") },
    ...(tunnel
      ? ([
          {
            kind: "item",
            label: "Check public access",
            icon: ShieldCheckIcon,
            disabled: verify.isPending,
            onSelect: () => verify.mutate(),
          },
        ] satisfies Entry[])
      : []),
    ...(available && !node.installations.some(updating)
      ? ([
          {
            kind: "item",
            label: `Update to ${available}`,
            icon: ArrowClockwiseIcon,
            disabled: update.isPending,
            onSelect: () => update.mutate(),
          },
        ] satisfies Entry[])
      : []),
    { kind: "separator" },
    ...(stopped
      ? ([
          {
            kind: "item",
            label: "Resume",
            icon: PlayIcon,
            disabled: resume.isPending,
            onSelect: () => resume.mutate(),
          },
        ] satisfies Entry[])
      : []),
    { kind: "item", label: "Drain", icon: PauseIcon, onSelect: () => setDraining("drain") },
    { kind: "item", label: "Drain and move", icon: ArrowsLeftRightIcon, onSelect: () => setDraining("move") },
    { kind: "item", label: "Remove server", icon: TrashIcon, destructive: true, onSelect: () => setRemoving(true) },
  ]
  return (
    <RowMenus
      label={`Actions for ${node.name}`}
      entries={entries}
      row={(actions) => (
        <tr className="transition-colors hover:bg-surface data-popup-open:bg-surface [&>td]:h-14 [&>td]:px-3">
          <td className="truncate font-medium">{node.name}</td>
          <td className="hidden truncate text-muted-foreground md:table-cell">{node.region}</td>
          <td className="hidden truncate text-muted-foreground md:table-cell">{runs(node)}</td>
          <td className="hidden truncate font-mono text-code text-muted-foreground sm:table-cell">{node.host}</td>
          <td>
            <span className="flex items-center gap-2 text-muted-foreground">
              <StatusDot online={state.ok} />
              <span className="truncate" title={state.detail}>
                {verify.isPending ? "Checking public access…" : state.label}
              </span>
              {!state.detail && available && (
                <span className="hidden truncate text-xs xl:inline">Update available: {available}</span>
              )}
            </span>
          </td>
          <td className="hidden truncate text-muted-foreground lg:table-cell">
            {seen && <Time iso={seen} relative />}
          </td>
          <td className="text-right">{actions}</td>
        </tr>
      )}
      dialogs={
        <>
          <ConfirmAction
            open={draining !== undefined}
            onOpenChange={(open) => !open && setDraining(undefined)}
            title={draining === "move" ? `Drain ${node.name} and move?` : `Drain ${node.name}?`}
            description={
              draining === "move"
                ? "Existing connections move to your other servers in this region now (or the backup region if there is none)."
                : "New connections stop; existing ones finish. Servers in the same region keep serving."
            }
            action={draining === "move" ? "Drain and move" : "Drain"}
            pending="Draining…"
            mutation={{
              mutationFn: () => drainInstallation(node.installations[0], draining === "move"),
              onSuccess: async () => {
                await refresh()
                toast.success(`${node.name} is draining`)
              },
            }}
          />
          <ConfirmAction
            open={removing}
            onOpenChange={setRemoving}
            title={`Remove ${node.name}?`}
            description="This server stops carrying traffic now. Machines switch to other servers your settings allow. To use it again, pair it with a new code."
            action="Remove server"
            pending="Removing…"
            confirmName={node.name}
            mutation={{
              mutationFn: async () => {
                for (const installation of node.installations) await removeInstallation(installation)
              },
              onSuccess: async () => {
                await refresh()
                toast.success(`${node.name} removed`)
              },
            }}
          />
        </>
      }
    />
  )
}

const regionStatus = { ready: "Ready", draining: "Being replaced", unavailable: "Not reachable" } as const

/** Latency in ms, blank without a fresh measurement from your machines. */
const latency = (ms?: number) => (ms === undefined ? "" : `${ms} ms`)

/** One row of the Relays or Edges table: (its role,) the region, whether it's ready, (latency). */
function RegionLine({
  role,
  name,
  status,
  rtt,
  showLatency,
}: {
  role?: string
  name: string
  status: keyof typeof regionStatus
  rtt?: number
  showLatency?: boolean
}) {
  return (
    <tr className="[&>td]:h-14 [&>td]:px-3">
      {role && <td className="truncate font-medium">{role}</td>}
      <td className="truncate">{name}</td>
      <td>
        <span className="flex items-center gap-2 text-muted-foreground">
          <StatusDot online={status === "ready"} />
          {regionStatus[status]}
        </span>
      </td>
      {showLatency && <td className="text-right font-mono text-code text-muted-foreground">{latency(rtt)}</td>}
    </tr>
  )
}

/** The Relays and Edges tables: (Role,) Region, Status, (Latency). Regions only: no node, host or operator. */
function RegionTable({
  roles,
  showLatency,
  loading,
  rows,
  children,
}: {
  roles?: boolean
  showLatency?: boolean
  loading: boolean
  rows: number
  children: React.ReactNode
}) {
  return (
    <table className="w-full table-fixed text-sm">
      <colgroup>
        {roles && <col className="w-[22%]" />}
        <col />
        <col className="w-[26%]" />
        {showLatency && <col className="w-[14%]" />}
      </colgroup>
      <thead className="border-b border-divider text-left text-xs text-muted-foreground">
        <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
          {roles && <th>Role</th>}
          <th>Region</th>
          <th>Status</th>
          {showLatency && <th className="text-right">Latency</th>}
        </tr>
      </thead>
      <tbody className="divide-y divide-divider border-b border-divider">
        {loading ? (
          <TableRowsSkeleton
            rows={rows}
            columns={[
              roles ? { bar: "w-24" } : false,
              { bar: "w-24" },
              { bar: "w-14", dot: true },
              showLatency ? { className: "text-right", bar: "w-10" } : false,
            ]}
          />
        ) : (
          children
        )}
      </tbody>
    </table>
  )
}

/**
 * Paperboat's relay regions. With a relay of your own, a switch keeps relayed traffic on your relays
 * only (the account's relay pool); your own relays are listed under Your servers.
 */
function RelaysSection({ scope }: { scope: ReturnType<typeof scopeOf> }) {
  const queryClient = useQueryClient()
  const relays = useApiQuery(relaysQuery)
  const pool = useApiQuery(poolQuery("relay", scope))
  const own = useInfiniteQuery(poolServersQuery("relay", scope, ""))
  const ownIds = (own.data?.pages.flatMap((page) => page.items) ?? []).map((server) => server.installation_id)
  const only = pool.data?.mode === "self-hosted-only"
  const save = useMutation({
    mutationFn: (on: boolean) =>
      savePool(
        "relay",
        scope,
        on
          ? { mode: "self-hosted-only", installation_ids: ownIds.slice(0, 32) }
          : { mode: "mixed", installation_ids: [] },
      ),
    onSuccess: async (_, on) => {
      await refreshNetwork(queryClient)
      toast.success(on ? "Relayed traffic uses only your relays" : "Relayed traffic can use Paperboat’s relays")
    },
    onError: (error) => errorToast("Couldn’t change relays", error),
  })
  // Ready regions first.
  const rows = [...(relays.data ?? [])].sort(
    (a, b) => Number(b.status === "ready") - Number(a.status === "ready") || a.name.localeCompare(b.name),
  )
  return (
    <section className="flex flex-col gap-3">
      <div className="flex flex-col gap-1">
        <SectionHeading
          title="Relays"
          action={
            ownIds.length > 0 &&
            pool.data && (
              <label className="flex items-center gap-2 text-sm">
                Use only my relays
                <Switch checked={only} disabled={save.isPending} onCheckedChange={(on) => save.mutate(on)} />
              </label>
            )
          }
        />
        <p className="text-sm text-muted-foreground">
          Used only when two machines can’t connect directly. Paperboat picks the fastest one for each connection.
        </p>
      </div>
      <RegionTable loading={relays.isPending} rows={2}>
        {rows.map((relay) => (
          <RegionLine key={relay.id} name={relay.name} status={relay.status} />
        ))}
      </RegionTable>
      {relays.isError && <LoadError what="relays" error={relays.error} retry={() => relays.refetch()} />}
    </section>
  )
}

/**
 * The workspace's pair of regions (GET /v1/edges/pair): where its tunnels and previews are served.
 * Machines pick between the two by what they measure, so the labels are roles, not a promise of
 * which one carries traffic. Owners and admins change the pair.
 */
function EdgesSection({ workspace, manage }: { workspace: string; manage: boolean }) {
  const pair = useApiQuery({ ...edgePairQuery(workspace), enabled: Boolean(workspace) })
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
            name={region.name}
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

/** A region to choose in Change regions: one of Paperboat's, or one of your own (grouping your servers). */
type RegionOption = {
  /** `kind:id`: a Paperboat region and one of your own can share an id. */
  key: string
  kind: PairKind
  name: string
  /** For your own regions, how many of your servers are in it. */
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
      return {
        key: `self_hosted:${id}`,
        kind: "self_hosted" as const,
        name: id,
        detail: region.servers === 1 ? "1 server" : `${region.servers} servers`,
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

/**
 * Add server, modeled on Add machine: the install command first, then the address and code it
 * prints. The node belongs to the selected workspace.
 */
function AddServerDialog({ initial, onClose }: { initial: "relay" | "tunnel"; onClose: () => void }) {
  const queryClient = useQueryClient()
  const workspace = useWorkspace()
  const [components, setComponents] = useShellState<"relay" | "tunnel" | "both">(
    "network/network-page.tsx:components",
    initial,
  )
  const installer = useApiQuery(clientConfigQuery).data?.installer_base_url
  const [address, setAddress] = useShellState("network/network-page.tsx:address", "")
  const [code, setCode] = useShellState("network/network-page.tsx:code", "")
  const pair = useMutation({
    mutationFn: () => pairNode({ address: address.trim(), code: code.trim(), scope: scopeOf(workspace) }),
    onSuccess: () => refreshNetwork(queryClient),
  })
  const ready = Boolean(address.trim() && code.trim())

  return (
    <Dialog open onOpenChange={(next) => !next && !pair.isPending && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Add a server</DialogTitle>
        </DialogHeader>
        {pair.data ? (
          <div className="flex flex-col items-center gap-3 py-4 text-center">
            <CheckCircleIcon weight="fill" className="size-10 text-success" />
            <p className="font-medium">{pair.data[0]?.name ?? "Server"} is paired</p>
            <ul className="flex flex-col gap-1 text-xs text-muted-foreground">
              {pair.data.map((installation) => (
                <li key={installation.installation_id}>
                  {installation.capability === "tunnel" ? "Tunnel server" : "Relay"}: {installation.trust_notice}
                </li>
              ))}
            </ul>
            <Button size="lg" className="mt-1" onClick={onClose}>
              Done
            </Button>
          </div>
        ) : (
          <form
            className="flex min-w-0 flex-col gap-4"
            onSubmit={(event) => {
              event.preventDefault()
              if (ready) pair.mutate()
            }}
          >
            <ToggleGroup
              variant="outline"
              spacing={0}
              value={[components]}
              onValueChange={(next) => next[0] && setComponents(next[0] as typeof components)}
              className="w-full"
            >
              <ToggleGroupItem value="relay" className="h-8 flex-1 text-sm">
                Relay
              </ToggleGroupItem>
              <ToggleGroupItem value="tunnel" className="h-8 flex-1 text-sm">
                Tunnel server
              </ToggleGroupItem>
              <ToggleGroupItem value="both" className="h-8 flex-1 text-sm">
                Both
              </ToggleGroupItem>
            </ToggleGroup>
            <div className="flex flex-col gap-2">
              {installer ? (
                <CommandBlock command={installCommand(installer, components)} />
              ) : (
                <Skeleton className="h-12 w-full" />
              )}
              <p className="text-xs text-muted-foreground">
                Run this on a Linux server with a public IP. It prints an address and a pairing code.
              </p>
            </div>

            <div className="flex flex-col gap-3 border-t border-divider pt-4">
              <Field className="gap-1.5">
                <FieldLabel htmlFor="node-address">Address</FieldLabel>
                <Input
                  id="node-address"
                  placeholder="203.0.113.7:8443"
                  value={address}
                  onChange={(event) => setAddress(event.target.value)}
                  className="font-mono"
                  autoComplete="off"
                  spellCheck={false}
                />
              </Field>
              <Field className="gap-1.5">
                <FieldLabel htmlFor="node-code">Code</FieldLabel>
                <Input
                  id="node-code"
                  value={code}
                  onChange={(event) => setCode(event.target.value)}
                  className="font-mono"
                  autoComplete="off"
                  spellCheck={false}
                />
                <p className="text-xs text-muted-foreground">
                  The code works once, for 15 minutes. Get a new one on the server with the command above.
                </p>
              </Field>
            </div>
            {pair.isError && (
              <p className="text-sm text-destructive">
                <ErrorMessage error={pair.error} />
              </p>
            )}
            <DialogFooter>
              <Button type="button" variant="secondary" size="lg" onClick={onClose} disabled={pair.isPending}>
                Cancel
              </Button>
              <Button type="submit" size="lg" disabled={!ready || pair.isPending}>
                {pair.isPending ? "Pairing…" : "Pair server"}
              </Button>
            </DialogFooter>
          </form>
        )}
      </DialogContent>
    </Dialog>
  )
}
