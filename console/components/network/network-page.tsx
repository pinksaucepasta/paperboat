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
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
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
      <Section
        title="How your traffic travels"
        action={
          <Button size="lg" onClick={() => setAdding("relay")}>
            <PlusIcon data-icon="inline-start" />
            Add server
          </Button>
        }
      >
        {/* The cards share row tracks (subgrid), so their rows line up whatever wraps. */}
        <div className="grid gap-x-3 gap-y-3 pt-3 sm:grid-cols-2">
          <PathCard capability="relay" scope={scope} team={team} onAdd={() => setAdding("relay")} />
          <PathCard capability="tunnel" scope={scope} team={team} onAdd={() => setAdding("tunnel")} />
        </div>
      </Section>

      <Section
        title={team ? `${workspace?.name ?? "Team"} servers` : "Your servers"}
        action={
          (q || (installations.data?.total ?? 0) > cursorPageSize) && (
            <SearchField value={search} onChange={setSearch} placeholder="Search servers" />
          )
        }
      >
        {none ? (
          <p className="border-b border-divider py-3 text-sm text-muted-foreground">
            None yet. Run a relay or tunnel server on a Linux machine to keep traffic on your hardware.
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
                  <th className="hidden md:table-cell">Runs</th>
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

      {adding && (
        <AddServerDialog
          workspaceName={workspace?.name ?? "Personal"}
          initial={adding}
          onClose={() => setAdding(undefined)}
        />
      )}
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
    about: "Used only when machines can’t connect directly. End-to-end encrypted.",
    shared: { mode: "mixed" as const, title: "Paperboat’s and yours" },
    own: "Machines that can’t connect directly use only your relays. If your relays are down, those connections fail.",
  },
  tunnel: {
    title: "Tunnels & previews",
    about: "Public addresses for your apps. Browser HTTPS ends here.",
    shared: { mode: "managed-only" as const, title: "Paperboat’s" },
    own: "Tunnels and previews run only on your servers and your own domains. Stop running previews and tunnels before switching.",
  },
}

type Health = { ok: boolean; label: string }

const health = (ready: number, total: number): Health =>
  ready === total
    ? { ok: true, label: "Ready" }
    : ready
      ? { ok: false, label: `${ready} of ${total} ready` }
      : { ok: false, label: total ? "Not ready" : "None chosen" }

/**
 * One way traffic travels (relays, or tunnels and previews): who serves it now and whether it's
 * healthy, and, once you have a server of that kind, the choice between Paperboat's and only yours.
 */
function PathCard({
  capability,
  scope,
  team,
  onAdd,
}: {
  capability: Capability
  scope: ReturnType<typeof scopeOf>
  team: boolean
  onAdd: () => void
}) {
  const queryClient = useQueryClient()
  const [search, setSearch] = useState("")
  const q = useDeferredValue(search.trim())
  const found = useInfiniteQuery(poolServersQuery(capability, scope, q))
  const servers = found.data?.pages.flatMap((page) => page.items) ?? []
  // Whether any server exists comes from the unfiltered total, not from what a search matched.
  const allServers = useInfiniteQuery(poolServersQuery(capability, scope, ""))
  const anyServer = allServers.data?.pages[0]?.total ?? 0
  const known = allServers.data?.pages.flatMap((page) => page.items) ?? []
  const edges = useApiQuery({ ...edgesQuery, enabled: capability === "tunnel" })
  const relays = useApiQuery({ ...relaysQuery, enabled: capability === "relay" })
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

  // Who serves it now (from the saved setting, not the unsaved draft), one detail line under it, and
  // its health where it is known. Both cards keep the same rows in the same order.
  const plural = (count: number) => `${count} ${count === 1 ? "server" : "servers"}`
  let served: React.ReactNode = <Skeleton className="my-1 h-3 w-40" />
  let detail: React.ReactNode = <Skeleton className="my-1 h-3 w-24" />
  let status: Health | undefined
  if (pool.data?.mode === "self-hosted-only") {
    const members = pool.data.members ?? []
    served = members.map((member) => member.name).join(", ") || "Your servers"
    detail = `Only yours · ${plural(pool.data.installation_ids.length)}`
    const ready = known.filter((server) => pool.data!.installation_ids.includes(server.installation_id) && server.ready)
    status = health(ready.length, pool.data.installation_ids.length)
  } else if (capability === "tunnel" && edges.data) {
    const regions = [...new Set(edges.data.map((edge) => placeName(edge.region)))]
    served = `Paperboat · ${regions.join(", ") || "no region"}`
    detail = plural(edges.data.length)
    status = health(edges.data.filter((edge) => edge.status === "ready").length, edges.data.length)
  } else if (capability === "relay" && relays.data) {
    // Every relay the settings let these machines use, Paperboat's and yours, with its heartbeat.
    const regions = [
      ...new Set(relays.data.filter((relay) => !relay.self_hosted).map((relay) => placeName(relay.region))),
    ]
    served = `${anyServer ? "Paperboat’s and yours" : "Paperboat"} · ${regions.join(", ") || "no region"}`
    detail = plural(relays.data.length)
    status = health(relays.data.filter((relay) => relay.status === "ready").length, relays.data.length)
  }
  if (edges.isError || relays.isError || pool.isError) detail = "Couldn’t load"

  return (
    <div className="row-span-4 grid grid-rows-subgrid gap-y-4 surface rounded-lg p-4 text-sm">
      <div className="flex flex-col gap-1">
        <span className="font-medium">{copy.title}</span>
        <span className="text-muted-foreground">{copy.about}</span>
      </div>
      <div className="border-t border-divider" />
      <div className="flex flex-col gap-0.5">
        <div className="flex h-5 items-center justify-between gap-3">
          <span className="min-w-0 truncate">{served}</span>
          {status ? (
            <span className="flex shrink-0 items-center gap-2 text-muted-foreground">
              <StatusDot online={status.ok} />
              {status.label}
            </span>
          ) : (
            !(edges.isError || relays.isError || pool.isError) && <Skeleton className="my-1 h-3 w-14 shrink-0" />
          )}
        </div>
        <span className="flex h-5 items-center text-muted-foreground">{detail}</span>
      </div>
      {/* Bottom-aligned, so "Use your own server" (or the choice) sits at the same height in both cards. */}
      <div className="flex flex-col justify-end gap-4">
        {capability === "relay" && team && (
          <p className="text-muted-foreground">Applies to your account in every workspace.</p>
        )}
        {pool.isError ? (
          <p className="text-muted-foreground">Couldn’t load this setting. {pool.error.message}</p>
        ) : !anyServer && !own ? (
          <button
            type="button"
            onClick={onAdd}
            className="self-start underline decoration-muted-foreground/50 underline-offset-4 outline-none hover:decoration-foreground focus-visible:ring-2 focus-visible:ring-ring/30"
          >
            Use your own server
          </button>
        ) : (
          current && (
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
          )
        )}
        {own && draft && (
          <div className="flex flex-col gap-2">
            <p className="text-muted-foreground">{copy.own}</p>
            <div className="flex items-center justify-between gap-3">
              <SearchField value={search} onChange={setSearch} placeholder="Search servers" />
              <span className="shrink-0 text-muted-foreground">
                {current!.installation_ids.length} of 32 · {found.data?.pages[0]?.total ?? 0} {q ? "found" : "servers"}
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
                    <StatusDot online={server.ready} />
                  </label>
                </li>
              ))}
            </ul>
            {found.hasNextPage && (
              <div className="flex items-center gap-3">
                <Button variant="secondary" onClick={() => found.fetchNextPage()} disabled={found.isFetchingNextPage}>
                  {found.isFetchingNextPage ? "Loading…" : "Load more"}
                </Button>
                {found.isFetchNextPageError && <span className="text-destructive">Couldn’t load more. Try again.</span>}
              </div>
            )}
          </div>
        )}
        {save.isError && <p className="text-destructive">{save.error.message}</p>}
        {dirty && (
          <div className="flex gap-2">
            <Button
              onClick={() => save.mutate()}
              disabled={save.isPending || (own && !current!.installation_ids.length)}
            >
              {save.isPending ? "Saving…" : "Save"}
            </Button>
            <Button variant="secondary" onClick={() => setDraft(undefined)} disabled={save.isPending}>
              Cancel
            </Button>
          </div>
        )}
      </div>
    </div>
  )
}

/**
 * Add server, modeled on Add machine: the install command first, then the address and code it
 * prints. The node belongs to the selected workspace.
 */
function AddServerDialog({
  workspaceName,
  initial,
  onClose,
}: {
  workspaceName: string
  initial: "relay" | "tunnel"
  onClose: () => void
}) {
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
            <p className="text-xs text-muted-foreground">It carries traffic once you choose it in “How your traffic travels”.</p>
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
