"use client"

import { CheckCircleIcon, CopyIcon, HardDrivesIcon, PlusIcon, ShieldCheckIcon, TrashIcon } from "@phosphor-icons/react"
import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query"
import { useDeferredValue, useState } from "react"
import { toast } from "sonner"
import { clientConfigQuery, edgesQuery, useApiQuery, useWorkspace } from "@/lib/api"
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
  type Capability,
  type Node,
  type Pool,
} from "@/lib/network"
import { usePageParam } from "@/lib/use-page-param"
import { CommandBlock } from "@/components/add-machine-dialog"
import { ListPager } from "@/components/list-pager"
import { ConfirmAction, SearchField } from "@/components/list-toolbar"
import { StatusDot } from "@/components/machine-status"
import { RowMenus, type Entry } from "@/components/row-menus"
import { Section } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"

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
  const [search, setSearch] = useState("")
  const q = useDeferredValue(search.trim())
  const pages = usePageParam(q)
  const installations = useApiQuery({ ...installationsQuery(scope, q, pages.page), enabled: Boolean(workspace) })
  const [adding, setAdding] = useState(false)
  const nodes = groupNodes(installations.data?.items ?? [])
  const none = installations.isSuccess && installations.data.total === 0 && !q
  const team = workspace?.kind === "team"
  const addButton = (
    <Button size="lg" onClick={() => setAdding(true)}>
      <PlusIcon data-icon="inline-start" />
      Add server
    </Button>
  )

  return (
    <div className="flex flex-col gap-10">
      {none ? (
        <Empty className="py-6">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <HardDrivesIcon />
            </EmptyMedia>
            <EmptyTitle>{team ? `No servers in ${workspace?.name}` : "No servers of your own"}</EmptyTitle>
            <EmptyDescription>
              {team ? "The team’s machines" : "Your machines"} use Paperboat’s servers. Run a relay or tunnel server on
              your own Linux server to keep traffic on hardware you control.
            </EmptyDescription>
          </EmptyHeader>
          <EmptyContent>{addButton}</EmptyContent>
        </Empty>
      ) : (
        <section className="flex flex-col gap-3">
          <div className="flex items-center gap-2">
            {(q || (installations.data?.total ?? 0) > cursorPageSize) && (
              <SearchField value={search} onChange={setSearch} placeholder="Search servers" />
            )}
            <div className="ml-auto">{addButton}</div>
          </div>
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
                  <th>Your servers</th>
                  <th className="hidden md:table-cell">Runs</th>
                  <th className="hidden sm:table-cell">Address</th>
                  <th>Status</th>
                  <th className="w-12" aria-label="Actions" />
                </tr>
              </thead>
              <tbody className="divide-y divide-divider border-b border-divider">
                {installations.isPending
                  ? Array.from({ length: 2 }, (_, index) => (
                      <tr key={index} className="[&>td]:h-14 [&>td]:px-3">
                        <td colSpan={5}>
                          <Skeleton className="h-5 w-1/3" />
                        </td>
                      </tr>
                    ))
                  : nodes.map((node) => <NodeRow key={node.id} node={node} />)}
              </tbody>
            </table>
            {installations.data && (
              <ListPager page={pages.page} size={cursorPageSize} total={installations.data.total} go={pages.go} />
            )}
            {installations.isError && (
              <div className="flex items-center justify-center gap-3 py-12 text-sm text-muted-foreground">
                Couldn’t load your servers.
                <Button variant="outline" size="sm" onClick={() => installations.refetch()}>
                  Retry
                </Button>
              </div>
            )}
            {installations.isSuccess && nodes.length === 0 && q && (
              <p className="py-12 text-center text-sm text-muted-foreground">No servers match “{q}”</p>
            )}
          </div>
        </section>
      )}

      <PaperboatServers />
      <Section title="Servers in use">
        <PoolRow capability="relay" scope={scope} team={team} />
        <PoolRow capability="tunnel" scope={scope} team={team} />
      </Section>

      {adding && <AddServerDialog workspaceName={workspace?.name ?? "Personal"} onClose={() => setAdding(false)} />}
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
        <tr className="data-popup-open:bg-surface [&>td]:h-14 [&>td]:px-3">
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
          description="It stops carrying your traffic right away. Machines fall back to other servers where your settings allow. To use it again, pair it with a fresh code."
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

const poolCopy = {
  relay: {
    title: "Relays",
    about: "Carry traffic only when two machines can’t connect directly. They never see inside the encryption.",
    shared: { mode: "mixed" as const, title: "Paperboat’s and yours" },
    own: "Traffic that can’t go directly uses only the relays you pick. If they’re down, it fails instead of using Paperboat’s.",
  },
  tunnel: {
    title: "Tunnels and previews",
    about: "Where public addresses meet the internet. Browser HTTPS ends on this server.",
    shared: { mode: "managed-only" as const, title: "Paperboat’s" },
    own: "Tunnels and previews are served only from the servers you pick, on your own domains. Stop running previews and tunnels before switching.",
  },
}

/** Which servers carry one kind of traffic: Paperboat's (and yours, for relays), or only yours. */
function PoolRow({
  capability,
  scope,
  team,
}: {
  capability: Capability
  scope: ReturnType<typeof scopeOf>
  team: boolean
}) {
  const queryClient = useQueryClient()
  const [search, setSearch] = useState("")
  const q = useDeferredValue(search.trim())
  const found = useInfiniteQuery(poolServersQuery(capability, scope, q))
  const servers = found.data?.pages.flatMap((page) => page.items) ?? []
  // Whether any server exists comes from the unfiltered total, not from what a search matched.
  const anyServer = useInfiniteQuery(poolServersQuery(capability, scope, "")).data?.pages[0]?.total ?? 0
  const copy = poolCopy[capability]
  const pool = useApiQuery(poolQuery(capability, scope))
  const [draft, setDraft] = useState<Pool>()
  const current = draft ?? pool.data
  const own = current?.mode === "self-hosted-only"
  const dirty = Boolean(draft) && JSON.stringify(draft) !== JSON.stringify(pool.data)
  const save = useMutation({
    mutationFn: () => savePool(capability, scope, draft!),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["selfhost", "pool", capability] })
      setDraft(undefined)
      toast.success(`${copy.title} updated`)
    },
  })
  const choose = (next: "shared" | "own") =>
    setDraft(
      next === "shared"
        ? { mode: copy.shared.mode, installation_ids: [] }
        : {
            mode: "self-hosted-only",
            installation_ids: current?.installation_ids.length
              ? current.installation_ids
              : servers.map((server) => server.installation_id).slice(0, 32),
          },
    )
  const toggle = (id: string, on: boolean) =>
    current &&
    setDraft({
      mode: "self-hosted-only",
      installation_ids: on
        ? [...current.installation_ids, id].slice(0, 32)
        : current.installation_ids.filter((entry) => entry !== id),
    })
  const kind = capability === "relay" ? "relay" : "tunnel server"

  return (
    <div className="flex flex-col gap-3 border-b border-divider py-4 text-sm">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between sm:gap-8">
        <div className="flex max-w-lg flex-col gap-1">
          <span className="font-medium">{copy.title}</span>
          <span className="text-muted-foreground">
            {copy.about}
            {capability === "relay" && team && " This is set for your account, in every workspace."}
          </span>
        </div>
        {pool.isError ? (
          <span className="text-muted-foreground">Couldn’t load this setting. {pool.error.message}</span>
        ) : !current ? (
          <Skeleton className="h-8 w-48" />
        ) : !anyServer && !own ? (
          <span className="flex flex-col gap-1 sm:items-end">
            <span>Paperboat’s</span>
            <span className="text-xs text-muted-foreground">Pair a {kind} to use your own</span>
          </span>
        ) : (
          <ToggleGroup
            variant="outline"
            spacing={0}
            value={[own ? "own" : "shared"]}
            onValueChange={(next) => next[0] && choose(next[0] as "shared" | "own")}
            aria-label={copy.title}
          >
            <ToggleGroupItem value="shared">{copy.shared.title}</ToggleGroupItem>
            <ToggleGroupItem value="own">Only yours</ToggleGroupItem>
          </ToggleGroup>
        )}
      </div>
      {own && (
        <div className="flex flex-col gap-2">
          <p className="text-muted-foreground">{copy.own}</p>
          <div className="flex items-center justify-between gap-3">
            <SearchField value={search} onChange={setSearch} placeholder="Search servers" />
            <span className="text-muted-foreground">
              {current!.installation_ids.length} of 32 chosen · {found.data?.pages[0]?.total ?? 0}{" "}
              {q ? "found" : "servers"}
            </span>
          </div>
          <ul className="flex flex-col divide-y divide-divider border-y border-divider">
            {current!.installation_ids
              .filter((id) => !servers.some((server) => server.installation_id === id))
              .map((id) => {
                const member = pool.data?.members?.find((entry) => entry.installation_id === id)
                return (
                  <li key={id}>
                    <label className="flex min-h-11 cursor-pointer items-center gap-3">
                      <Checkbox checked onCheckedChange={(on) => toggle(id, on)} />
                      <span className="min-w-0 flex-1 truncate">{member?.name ?? "Server no longer paired"}</span>
                      {member && (
                        <span className="hidden truncate font-mono text-code text-muted-foreground sm:inline">
                          {member.endpoint_host}
                        </span>
                      )}
                    </label>
                  </li>
                )
              })}
            {servers.map((server) => (
              <li key={server.installation_id}>
                <label className="flex min-h-11 cursor-pointer items-center gap-3">
                  <Checkbox
                    checked={current!.installation_ids.includes(server.installation_id)}
                    disabled={
                      !current!.installation_ids.includes(server.installation_id) &&
                      current!.installation_ids.length >= 32
                    }
                    onCheckedChange={(on) => toggle(server.installation_id, on)}
                  />
                  <span className="min-w-0 flex-1 truncate">{server.name}</span>
                  <span className="hidden truncate font-mono text-code text-muted-foreground sm:inline">
                    {server.endpoint_host}
                  </span>
                  <StatusDot online={server.ready} />
                </label>
              </li>
            ))}
          </ul>
          {found.hasNextPage && (
            <Button
              variant="secondary"
              className="self-start"
              onClick={() => found.fetchNextPage()}
              disabled={found.isFetchingNextPage}
            >
              {found.isFetchingNextPage ? "Loading…" : "Load more"}
            </Button>
          )}
        </div>
      )}
      {save.isError && <p className="text-destructive">{save.error.message}</p>}
      {dirty && (
        <div className="flex gap-2">
          <Button onClick={() => save.mutate()} disabled={save.isPending || (own && !current!.installation_ids.length)}>
            {save.isPending ? "Saving…" : "Save"}
          </Button>
          <Button variant="secondary" onClick={() => setDraft(undefined)} disabled={save.isPending}>
            Cancel
          </Button>
        </div>
      )}
    </div>
  )
}

/**
 * The edges Paperboat serves this account's tunnels and previews from, one row per place. Paperboat's
 * relays are not listed: no API names them for the browser.
 */
function PaperboatServers() {
  const edges = useApiQuery(edgesQuery)
  if (edges.isPending)
    return (
      <Section title="Paperboat’s servers">
        <div className="flex min-h-12 items-center border-b border-divider">
          <Skeleton className="h-4 w-1/3" />
        </div>
      </Section>
    )
  if (edges.isError)
    return (
      <Section title="Paperboat’s servers">
        <p className="border-b border-divider py-3 text-sm text-muted-foreground">
          Couldn’t load Paperboat’s servers. {edges.error.message}
        </p>
      </Section>
    )
  if (!edges.data.length) return null
  const places = new Map<string, { total: number; ready: number }>()
  for (const edge of edges.data) {
    const place = places.get(edge.region) ?? { total: 0, ready: 0 }
    place.total++
    if (edge.status === "ready") place.ready++
    places.set(edge.region, place)
  }
  return (
    <Section title="Paperboat’s servers">
      {[...places].map(([region, place]) => (
        <div key={region} className="flex min-h-12 items-center gap-3 border-b border-divider text-sm">
          <span className="w-1/3 min-w-0 truncate">{placeName(region)}</span>
          <span className="hidden min-w-0 flex-1 truncate text-muted-foreground sm:block">
            Tunnels and previews{place.total > 1 && ` · ${place.total} servers`}
          </span>
          <span className="ml-auto flex items-center gap-2 text-muted-foreground">
            <StatusDot online={place.ready > 0} />
            {place.ready === place.total
              ? "Ready"
              : place.ready
                ? `${place.ready} of ${place.total} ready`
                : "Unavailable"}
          </span>
        </div>
      ))}
    </Section>
  )
}

/**
 * Add server, modeled on Add machine: the install command first, then the address and code it
 * prints. The node belongs to the selected workspace.
 */
function AddServerDialog({ workspaceName, onClose }: { workspaceName: string; onClose: () => void }) {
  const queryClient = useQueryClient()
  const workspace = useWorkspace()
  const [components, setComponents] = useState<"relay" | "tunnel" | "both">("relay")
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
            <p className="text-xs text-muted-foreground">It carries traffic once you choose it under “Servers in use”.</p>
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
                Run it on a Linux server with a public address. It prints an address and a code for the next step.
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
                  Works once, for 15 minutes; run pbh selfhost code on the server for a new one. The server joins{" "}
                  {workspaceName}.
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
