"use client"

import { CaretUpDownIcon, CheckCircleIcon, CopyIcon, PlusIcon, ShieldCheckIcon, TrashIcon } from "@phosphor-icons/react"
import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query"
import { useDeferredValue, useState } from "react"
import { toast } from "sonner"
import { ApiError, clientConfigQuery, edgesQuery, relaysQuery, useApiQuery, useWorkspace } from "@/lib/api"
import { copyWithToast } from "@/lib/clipboard"
import { cursorPageSize } from "@/lib/cursor-pages"
import { placeName } from "@/lib/format"
import {
  edgePairQuery,
  groupNodes,
  installCommand,
  installationsQuery,
  pairNode,
  poolQuery,
  poolServersQuery,
  removeInstallation,
  saveEdgePair,
  savePool,
  scopeOf,
  verifyIngress,
  type EdgePair,
  type PairEdge,
  type Node,
} from "@/lib/network"
import { usePageParam } from "@/lib/use-page-param"
import { CommandBlock } from "@/components/add-machine-dialog"
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

/*
 * Your own relays and tunnel servers, and which servers your traffic uses. Machines connect
 * directly whenever they can; a relay carries what can't go directly (it never sees inside the
 * encryption); a tunnel server is where your tunnels and previews meet the internet (browser HTTPS
 * ends there).
 */

const runs = (node: Node) => {
  const kinds = new Set(node.installations.map((installation) => installation.capability))
  return kinds.size === 2 ? "Relay and tunnel server" : kinds.has("tunnel") ? "Tunnel server" : "Relay"
}

function nodeState(node: Node): { ok: boolean; label: string } {
  if (node.installations.some((installation) => installation.enrollment_state === "pending"))
    return { ok: false, label: "Finishing pairing" }
  const notReady = node.installations.filter((installation) => !installation.ready)
  if (!notReady.length) return { ok: true, label: "Ready" }
  if (notReady.length === node.installations.length) return { ok: false, label: "Not ready" }
  return { ok: false, label: `${notReady[0].capability === "tunnel" ? "Tunnel server" : "Relay"} not ready` }
}

export function NetworkPage() {
  const workspace = useWorkspace()
  const scope = scopeOf(workspace)
  const team = workspace?.kind === "team"
  const [search, setSearch] = useState("")
  const q = useDeferredValue(search.trim())
  const pages = usePageParam(q)
  const installations = useApiQuery({ ...installationsQuery(scope, q, pages.page), enabled: Boolean(workspace) })
  const [adding, setAdding] = useState<"relay" | "tunnel" | undefined>()
  const nodes = groupNodes(installations.data?.items ?? [])
  const none = installations.isSuccess && installations.data.total === 0 && !q

  return (
    <div className="flex flex-col gap-8">
      <RelaysSection scope={scope} />
      <EdgesSection
        workspace={workspace?.id ?? ""}
        manage={workspace?.kind === "personal" || workspace?.role === "owner" || workspace?.role === "admin"}
      />

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
                <col className="w-[24%]" />
                <col className="hidden w-[18%] md:table-column" />
                <col className="hidden sm:table-column" />
                <col className="w-[20%]" />
                <col className="hidden w-[14%] lg:table-column" />
                <col className="w-12" />
              </colgroup>
              <thead className="border-b border-divider text-left text-xs text-muted-foreground">
                <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
                  <th>Server</th>
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
  const state = nodeState(node)
  const tunnel = node.installations.find((installation) => installation.capability === "tunnel")
  // The newest heartbeat of the node's components.
  const seen = node.installations
    .map((installation) => installation.last_heartbeat_at)
    .filter(Boolean)
    .sort()
    .at(-1)
  const refresh = () => queryClient.invalidateQueries({ queryKey: ["selfhost"] })
  const verify = useMutation({
    mutationFn: () => verifyIngress(tunnel!),
    onSuccess: async () => {
      await refresh()
      toast.success(`${node.name} answers from the internet`)
    },
    onError: (error) => toast.error("Public access check failed", { description: error.message }),
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
    { kind: "separator" },
    { kind: "item", label: "Remove server", icon: TrashIcon, destructive: true, onSelect: () => setRemoving(true) },
  ]
  return (
    <RowMenus
      label={`Actions for ${node.name}`}
      entries={entries}
      row={(actions) => (
        <tr className="transition-colors hover:bg-surface data-popup-open:bg-surface [&>td]:h-14 [&>td]:px-3">
          <td className="truncate font-medium">{node.name}</td>
          <td className="hidden truncate text-muted-foreground md:table-cell">{runs(node)}</td>
          <td className="hidden truncate font-mono text-code text-muted-foreground sm:table-cell">{node.host}</td>
          <td>
            <span className="flex items-center gap-2 text-muted-foreground">
              <StatusDot online={state.ok} />
              <span className="truncate">{verify.isPending ? "Checking public access…" : state.label}</span>
            </span>
          </td>
          <td className="hidden truncate text-muted-foreground lg:table-cell">
            {seen && <Time iso={seen} relative />}
          </td>
          <td className="text-right">{actions}</td>
        </tr>
      )}
      dialogs={
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
      }
    />
  )
}

const nodeStatus = { ready: "Ready", draining: "Being replaced", unavailable: "Not reachable" } as const

/** Latency in ms, blank without a fresh measurement from your machines. */
const latency = (ms?: number) => (ms === undefined ? "" : `${ms} ms`)

/**
 * What tells two nodes in the same region apart: its host when the API gives one, else the start
 * of its id. shortcut: the API has no site name yet (requested); switch to it when it lands.
 */
const shortId = (id: string) => id.replace(/^node_/, "").slice(0, 8)

/** One row of the Relays or Edges table: (its role,) where it is, who runs it, whether it's ready, latency. */
function NodeLine({
  role,
  location,
  detail,
  operator,
  ok,
  status,
  rtt,
}: {
  role?: string
  location: string
  /** What tells it apart from another node in the same place (its host or a short id). */
  detail: string
  operator: string
  ok: boolean
  status: string
  rtt?: number
}) {
  return (
    <tr className="[&>td]:h-14 [&>td]:px-3">
      {role && <td className="truncate font-medium">{role}</td>}
      <td>
        <div className="truncate">{location}</div>
        <div className="truncate font-mono text-xs text-muted-foreground">{detail}</div>
      </td>
      <td className="truncate text-muted-foreground">{operator}</td>
      <td>
        <span className="flex items-center gap-2 text-muted-foreground">
          <StatusDot online={ok} />
          {status}
        </span>
      </td>
      <td className="text-right font-mono text-code text-muted-foreground">{latency(rtt)}</td>
    </tr>
  )
}

/** The Relays and Edges tables: (Edge,) Location, Operator, Status, Latency. */
function NodeTable({
  roles,
  loading,
  rows,
  children,
}: {
  roles?: boolean
  loading: boolean
  rows: number
  children: React.ReactNode
}) {
  return (
    <table className="w-full table-fixed text-sm">
      <colgroup>
        {roles && <col className="w-[18%]" />}
        <col />
        <col className="w-[26%]" />
        <col className="w-[20%]" />
        <col className="w-[14%]" />
      </colgroup>
      <thead className="border-b border-divider text-left text-xs text-muted-foreground">
        <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
          {roles && <th>Edge</th>}
          <th>Location</th>
          <th>Operator</th>
          <th>Status</th>
          <th className="text-right">Latency</th>
        </tr>
      </thead>
      <tbody className="divide-y divide-divider border-b border-divider">
        {loading ? (
          <TableRowsSkeleton
            rows={rows}
            columns={[
              roles ? { bar: "w-16" } : false,
              { bar: "w-24", second: "w-20" },
              { bar: "w-20" },
              { bar: "w-14", dot: true },
              { className: "text-right", bar: "w-10" },
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
 * Every relay the workspace's machines can use, Paperboat's and yours, ready ones first. With a
 * relay of your own, a switch keeps relayed traffic on your relays only (the account's relay pool).
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
      await queryClient.invalidateQueries({ queryKey: ["selfhost", "pool", "relay"] })
      toast.success(on ? "Relayed traffic uses only your relays" : "Relayed traffic can use Paperboat’s relays")
    },
    onError: (error) => toast.error(`Couldn’t change relays: ${error.message}`),
  })
  // Fastest first; unmeasured ones after, ready before not.
  const rows = [...(relays.data ?? [])].sort(
    (a, b) =>
      (a.best_rtt_ms ?? Infinity) - (b.best_rtt_ms ?? Infinity) ||
      Number(b.status === "ready") - Number(a.status === "ready") ||
      placeName(a.region).localeCompare(placeName(b.region)),
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
      <NodeTable loading={relays.isPending} rows={2}>
        {rows.map((relay) => (
          <NodeLine
            key={relay.id}
            location={placeName(relay.region)}
            detail={shortId(relay.id)}
            operator={relay.self_hosted ? (relay.name ?? "Your server") : "Paperboat"}
            ok={relay.status === "ready"}
            status={nodeStatus[relay.status]}
            rtt={relay.best_rtt_ms}
          />
        ))}
      </NodeTable>
      {relays.isError && <LoadError what="relays" error={relays.error} retry={() => relays.refetch()} />}
    </section>
  )
}

/**
 * The workspace's edge pair (GET /v1/edges/pair): where its tunnels and previews are served.
 * Machines pick between the two by what they measure, so the labels are roles, not a promise of
 * which one carries traffic. Owners and admins change the pair.
 */
function EdgesSection({ workspace, manage }: { workspace: string; manage: boolean }) {
  const pair = useApiQuery({ ...edgePairQuery(workspace), enabled: Boolean(workspace) })
  const [changing, setChanging] = useState(false)
  const edges: { role: string; edge: PairEdge }[] = []
  if (pair.data) edges.push({ role: "Primary", edge: pair.data.primary })
  if (pair.data?.fallback) edges.push({ role: "Fallback", edge: pair.data.fallback })
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
        <p className="text-sm text-muted-foreground">Your tunnels and previews are served from these two edges.</p>
      </div>
      <NodeTable roles loading={pair.isPending} rows={2}>
        {edges.map(({ role, edge }) => (
          <NodeLine
            key={role}
            role={role}
            location={placeName(edge.region)}
            detail={edge.endpoint_host ?? shortId(edge.id)}
            operator={edge.kind === "managed" ? "Paperboat" : (edge.name ?? "Your server")}
            ok={edge.status === "ready"}
            status={nodeStatus[edge.status]}
            rtt={edge.best_rtt_ms}
          />
        ))}
      </NodeTable>
      {pair.isSuccess && !pair.data && <p className="py-3 text-sm text-muted-foreground">No edges assigned yet.</p>}
      {pair.isError && <LoadError what="edges" error={pair.error} retry={() => pair.refetch()} />}
      {changing && pair.data !== undefined && (
        <ChangeEdgesDialog workspace={workspace} current={pair.data} onClose={() => setChanging(false)} />
      )}
    </section>
  )
}

/** An edge to choose in Change edges, described like its row in the Edges table. */
type EdgeOption = {
  id: string
  kind: "managed" | "self_hosted"
  location: string
  detail: string
  operator: string
  ok: boolean
  rtt?: number
  /** "Primary" or "Fallback" when it is one now. */
  role?: string
}

/** One edge as the table shows it: location over its host, operator, status dot, latency, current role. */
function EdgeLabel({ option }: { option: EdgeOption }) {
  return (
    <span className="flex min-w-0 flex-1 items-center gap-3 text-left">
      <span className="flex min-w-0 flex-1 flex-col">
        <span className="truncate">
          {option.location} <span className="text-muted-foreground">· {option.operator}</span>
        </span>
        <span className="truncate font-mono text-xs text-muted-foreground">{option.detail}</span>
      </span>
      {option.role && <span className="text-xs text-muted-foreground">{option.role}</span>}
      <StatusDot online={option.ok} />
      <span className="w-12 text-right font-mono text-code text-muted-foreground">{latency(option.rtt)}</span>
    </span>
  )
}

/** The Primary or Fallback field: the console's picker (popover + list), like choosing a machine. */
function EdgeChoice({
  id,
  options,
  value,
  onChange,
}: {
  id: string
  options: EdgeOption[]
  value: string
  onChange: (id: string) => void
}) {
  const [open, setOpen] = useState(false)
  const chosen = options.find((option) => option.id === value)
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
        {chosen ? <EdgeLabel option={chosen} /> : <span className="text-muted-foreground">Choose an edge</span>}
        <CaretUpDownIcon className="shrink-0 text-muted-foreground" />
      </PopoverTrigger>
      <PopoverContent align="start" className="w-(--anchor-width) gap-0 p-0">
        <Command>
          <CommandList>
            <CommandEmpty>No edges</CommandEmpty>
            <CommandGroup>
              {options.map((option) => (
                <CommandItem
                  key={option.id}
                  value={option.id}
                  className={pickerOption}
                  onSelect={() => {
                    onChange(option.id)
                    setOpen(false)
                  }}
                >
                  <EdgeLabel option={option} />
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
 * Picks the pair from Paperboat's edges and your own tunnel servers: both of one kind (the server
 * refuses a mix). Switching kinds needs running previews and tunnels stopped first (409).
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
  // The pair's edges carry their host and latency; other Paperboat edges only their id so far.
  const inPair = (id: string) =>
    current?.primary.id === id
      ? { edge: current.primary, role: "Primary" }
      : current?.fallback?.id === id
        ? { edge: current.fallback, role: "Fallback" }
        : undefined
  const options: EdgeOption[] = [
    ...(managed.data ?? []).map((edge) => {
      const known = inPair(edge.id)
      return {
        id: edge.id,
        kind: "managed" as const,
        location: placeName(edge.region),
        detail: known?.edge.endpoint_host ?? shortId(edge.id),
        operator: "Paperboat",
        ok: edge.status === "ready",
        rtt: known?.edge.best_rtt_ms,
        role: known?.role,
      }
    }),
    ...(own.data?.pages.flatMap((page) => page.items) ?? []).map((server) => {
      const known = inPair(server.installation_id)
      return {
        id: server.installation_id,
        kind: "self_hosted" as const,
        location: known ? placeName(known.edge.region) : server.name,
        detail: server.endpoint_host,
        operator: server.name,
        ok: server.ready,
        rtt: known?.edge.best_rtt_ms,
        role: known?.role,
      }
    }),
  ]
  const [primary, setPrimary] = useState(current?.primary.id ?? "")
  const [fallback, setFallback] = useState(current?.fallback?.id ?? "")
  const kindOf = (id: string) => options.find((option) => option.id === id)?.kind
  const problem =
    !primary || !fallback
      ? undefined
      : primary === fallback
        ? "Choose two different edges."
        : kindOf(primary) !== kindOf(fallback)
          ? "Choose two Paperboat edges or two of your servers."
          : undefined
  const save = useMutation({
    mutationFn: () => saveEdgePair(workspace, primary, fallback),
    onSuccess: (pair) => {
      queryClient.setQueryData(edgePairQuery(workspace).queryKey, pair)
      toast.success("Edges changed")
      onClose()
    },
  })
  const running =
    save.error instanceof ApiError && save.error.code === "previews_running"
      ? Number((save.error.details as { count?: number } | undefined)?.count ?? 0)
      : undefined
  const failure =
    running === undefined
      ? save.error?.message
      : running
        ? `Stop ${running} running ${running === 1 ? "preview or tunnel" : "previews and tunnels"} first.`
        : "Stop running previews and tunnels first."
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="sm:max-w-lg">
        <DialogHeader>
          <DialogTitle>Change edges</DialogTitle>
        </DialogHeader>
        <Field className="gap-1.5">
          <FieldLabel htmlFor="edge-primary">Primary</FieldLabel>
          <EdgeChoice id="edge-primary" options={options} value={primary} onChange={setPrimary} />
        </Field>
        <Field className="gap-1.5">
          <FieldLabel htmlFor="edge-fallback">Fallback</FieldLabel>
          <EdgeChoice id="edge-fallback" options={options} value={fallback} onChange={setFallback} />
        </Field>
        {(problem ?? failure) && <p className="text-sm text-destructive">{problem ?? failure}</p>}
        <DialogFooter>
          <Button variant="ghost" size="lg" onClick={onClose}>
            Cancel
          </Button>
          <Button
            size="lg"
            disabled={!primary || !fallback || Boolean(problem) || save.isPending}
            onClick={() => save.mutate()}
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
  const [components, setComponents] = useState<"relay" | "tunnel" | "both">(initial)
  const installer = useApiQuery(clientConfigQuery).data?.installer_base_url
  const [address, setAddress] = useState("")
  const [code, setCode] = useState("")
  const pair = useMutation({
    mutationFn: () => pairNode({ address: address.trim(), code: code.trim(), scope: scopeOf(workspace) }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["selfhost"] }),
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
            {pair.isError && <p className="text-sm text-destructive">{pair.error.message}</p>}
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
