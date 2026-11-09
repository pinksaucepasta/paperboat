"use client"

import { CheckCircleIcon, CopyIcon, PlusIcon, ShieldCheckIcon, TrashIcon } from "@phosphor-icons/react"
import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query"
import { useDeferredValue, useState } from "react"
import { toast } from "sonner"
import { clientConfigQuery, edgesQuery, relaysQuery, useApiQuery, useWorkspace } from "@/lib/api"
import { copyWithToast } from "@/lib/clipboard"
import { cursorPageSize } from "@/lib/cursor-pages"
import { placeName } from "@/lib/format"
import {
  groupNodes,
  installCommand,
  installationsQuery,
  pairNode,
  poolQuery,
  poolServersQuery,
  removeInstallation,
  savePool,
  scopeOf,
  verifyIngress,
  type Node,
} from "@/lib/network"
import { usePageParam } from "@/lib/use-page-param"
import { CommandBlock } from "@/components/add-machine-dialog"
import { ListPager } from "@/components/list-pager"
import { ConfirmAction, SearchField } from "@/components/list-toolbar"
import { StatusDot } from "@/components/machine-status"
import { RowMenus, type Entry } from "@/components/row-menus"
import { Section, SectionHeading } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
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
      <EdgesSection scope={scope} />

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
                <col className="w-[26%]" />
                <col className="hidden w-[22%] md:table-column" />
                <col className="hidden sm:table-column" />
                <col className="w-[22%]" />
                <col className="w-12" />
              </colgroup>
              <thead className="border-b border-divider text-left text-xs text-muted-foreground">
                <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
                  <th>Server</th>
                  <th className="hidden md:table-cell">Role</th>
                  <th className="hidden sm:table-cell">Address</th>
                  <th>Status</th>
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

/** One row of the Relays or Edges table: where it is, who runs it, and whether it's ready. */
function NodeLine({
  location,
  operator,
  ok,
  status,
}: {
  location: string
  operator: string
  ok: boolean
  status: string
}) {
  return (
    <tr className="[&>td]:h-12 [&>td]:px-3">
      <td className="truncate">{location}</td>
      <td className="truncate text-muted-foreground">{operator}</td>
      <td>
        <span className="flex items-center gap-2 text-muted-foreground">
          <StatusDot online={ok} />
          {status}
        </span>
      </td>
    </tr>
  )
}

/** The Relays and Edges tables: Location, Operator, Status. */
function NodeTable({ loading, rows, children }: { loading: boolean; rows: number; children: React.ReactNode }) {
  return (
    <table className="w-full table-fixed text-sm">
      <colgroup>
        <col />
        <col className="w-[34%]" />
        <col className="w-[28%]" />
      </colgroup>
      <thead className="border-b border-divider text-left text-xs text-muted-foreground">
        <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
          <th>Location</th>
          <th>Operator</th>
          <th>Status</th>
        </tr>
      </thead>
      <tbody className="divide-y divide-divider border-b border-divider">
        {loading ? (
          <TableRowsSkeleton rows={rows} columns={[{ bar: "w-24" }, { bar: "w-20" }, { bar: "w-14", dot: true }]} />
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
  const rows = [...(relays.data ?? [])].sort(
    (a, b) =>
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
            operator={relay.self_hosted ? (relay.name ?? "Your server") : "Paperboat"}
            ok={relay.status === "ready"}
            status={nodeStatus[relay.status]}
          />
        ))}
      </NodeTable>
      {relays.isError && <LoadError what="relays" error={relays.error} retry={() => relays.refetch()} />}
    </section>
  )
}

/**
 * Where the workspace's tunnels and previews are served: Paperboat's edges, or your own tunnel
 * servers when the workspace uses only those (the tunnel pool).
 */
function EdgesSection({ scope }: { scope: ReturnType<typeof scopeOf> }) {
  const pool = useApiQuery(poolQuery("tunnel", scope))
  const own = pool.data?.mode === "self-hosted-only"
  const edges = useApiQuery({ ...edgesQuery, enabled: pool.isSuccess && !own })
  const servers = useInfiniteQuery({ ...poolServersQuery("tunnel", scope, ""), enabled: own })
  const ready = new Set(
    (servers.data?.pages.flatMap((page) => page.items) ?? [])
      .filter((server) => server.ready)
      .map((server) => server.installation_id),
  )
  const loading = pool.isPending || (own ? servers.isPending : edges.isPending)
  return (
    <section className="flex flex-col gap-3">
      <div className="flex flex-col gap-1">
        <SectionHeading title="Edges" />
        <p className="text-sm text-muted-foreground">Your tunnels and previews are served from these two edges.</p>
      </div>
      <NodeTable loading={loading} rows={2}>
        {own
          ? (pool.data?.members ?? []).map((member) => (
              <NodeLine
                key={member.installation_id}
                location={member.endpoint_host}
                operator={member.name}
                ok={ready.has(member.installation_id)}
                status={ready.has(member.installation_id) ? "Ready" : "Not ready"}
              />
            ))
          : (edges.data ?? []).map((edge) => (
              <NodeLine
                key={edge.id}
                location={placeName(edge.region)}
                operator="Paperboat"
                ok={edge.status === "ready"}
                status={nodeStatus[edge.status]}
              />
            ))}
      </NodeTable>
      {(pool.isError || edges.isError) && (
        <LoadError
          what="edges"
          error={(pool.error ?? edges.error)!}
          retry={() => (pool.isError ? pool.refetch() : edges.refetch())}
        />
      )}
    </section>
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
            <p className="text-xs text-muted-foreground">
              Choose this server under “How your traffic travels” to start using it.
            </p>
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
