"use client"

import { CheckCircleIcon, CopyIcon, PlusIcon, ShieldCheckIcon, TrashIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { toast } from "sonner"
import { clientConfigQuery, edgesQuery, useApiQuery, useWorkspace } from "@/lib/api"
import { copyWithToast } from "@/lib/clipboard"
import { placeName } from "@/lib/format"
import {
  groupNodes,
  installCommand,
  installationsQuery,
  pairNode,
  poolQuery,
  removeInstallation,
  savePool,
  scopeOf,
  verifyIngress,
  type Capability,
  type Installation,
  type Node,
  type Pool,
} from "@/lib/network"
import { cn } from "@/lib/utils"
import { CommandBlock } from "@/components/add-machine-dialog"
import { ConfirmAction } from "@/components/list-toolbar"
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
  const installations = useApiQuery({ ...installationsQuery(scope), enabled: Boolean(workspace) })
  const [adding, setAdding] = useState(false)
  const nodes = groupNodes(installations.data ?? [])
  const enrolled = (capability: Capability) =>
    (installations.data ?? []).filter(
      (installation) => installation.capability === capability && installation.enrollment_state === "enrolled",
    )

  return (
    <div className="flex flex-col gap-10">
      <Section
        title="Your servers"
        action={
          <Button variant="secondary" size="sm" onClick={() => setAdding(true)}>
            <PlusIcon data-icon="inline-start" />
            Add server
          </Button>
        }
      >
        {installations.isPending ? (
          <div className="flex h-12 items-center border-b border-divider">
            <Skeleton className="h-4 w-1/3" />
          </div>
        ) : installations.isError ? (
          <p className="border-b border-divider py-3 text-sm text-muted-foreground">
            Couldn’t load your servers. {installations.error.message}
          </p>
        ) : nodes.length === 0 ? (
          <p className="border-b border-divider py-3 text-sm text-muted-foreground">
            Run a relay or tunnel server on your own Linux server, then pair it here.
          </p>
        ) : (
          <table className="w-full table-fixed text-sm">
            <colgroup>
              <col className="w-[24%]" />
              <col className="hidden w-[24%] md:table-column" />
              <col className="hidden sm:table-column" />
              <col className="w-[22%]" />
              <col className="w-12" />
            </colgroup>
            <tbody className="divide-y divide-divider border-b border-divider">
              {nodes.map((node) => (
                <NodeRow key={node.id} node={node} />
              ))}
            </tbody>
          </table>
        )}
      </Section>

      <PoolSection capability="relay" scope={scope} servers={enrolled("relay")} />
      <PoolSection capability="tunnel" scope={scope} servers={enrolled("tunnel")} />
      <PaperboatServers />

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
        <tr className="data-popup-open:bg-surface [&>td]:h-14 [&>td]:px-3 [&>td:first-child]:pl-0">
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
    shared: {
      mode: "mixed" as const,
      title: "Paperboat’s and yours",
      body: "Your relays are used alongside Paperboat’s. Machines still connect directly whenever they can.",
    },
    own: {
      title: "Only yours",
      body: "Traffic that can’t go directly uses only the relays you pick. If they’re down, it fails instead of using Paperboat’s.",
    },
  },
  tunnel: {
    title: "Tunnel servers",
    shared: {
      mode: "managed-only" as const,
      title: "Paperboat’s",
      body: "Tunnels and previews are served from Paperboat’s servers.",
    },
    own: {
      title: "Only yours",
      body: "Tunnels and previews are served only from the servers you pick, on your own domains. Stop running previews and tunnels before switching.",
    },
  },
}

/** Which servers carry one kind of traffic: Paperboat's (and yours, for relays), or only yours. */
function PoolSection({
  capability,
  scope,
  servers,
}: {
  capability: Capability
  scope: ReturnType<typeof scopeOf>
  servers: Installation[]
}) {
  const queryClient = useQueryClient()
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

  return (
    <Section title={copy.title}>
      {pool.isError ? (
        <p className="border-b border-divider py-3 text-sm text-muted-foreground">
          Couldn’t load this setting. {pool.error.message}
        </p>
      ) : (
        <div className="flex flex-col gap-3 pt-3">
          <div className="grid gap-2 sm:grid-cols-2" role="radiogroup" aria-label={copy.title}>
            {(["shared", "own"] as const).map((option) => {
              const selected = option === "own" ? own : Boolean(current) && !own
              const text = option === "own" ? copy.own : copy.shared
              const disabled = !current || (option === "own" && !servers.length)
              return (
                <button
                  key={option}
                  type="button"
                  role="radio"
                  aria-checked={selected}
                  disabled={disabled}
                  onClick={() => choose(option)}
                  className={cn(
                    "flex flex-col gap-1 rounded-lg border p-4 text-left text-sm transition-colors outline-none focus-visible:ring-2 focus-visible:ring-ring/30 disabled:cursor-not-allowed disabled:opacity-50",
                    selected ? "border-ring bg-ring/5" : "border-border hover:bg-surface",
                  )}
                >
                  <span className="flex items-center gap-2 font-medium">
                    {selected && <CheckCircleIcon weight="fill" className="size-4 text-primary" />}
                    {text.title}
                  </span>
                  <span className="text-muted-foreground">
                    {option === "own" && !servers.length
                      ? `Pair a ${capability === "relay" ? "relay" : "tunnel server"} first.`
                      : text.body}
                  </span>
                </button>
              )
            })}
          </div>
          {own && (
            <ul className="flex flex-col divide-y divide-divider border-y border-divider">
              {servers.map((server) => (
                <li key={server.installation_id}>
                  <label className="flex min-h-11 cursor-pointer items-center gap-3 text-sm">
                    <Checkbox
                      checked={current!.installation_ids.includes(server.installation_id)}
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
          )}
          {save.isError && <p className="text-sm text-destructive">{save.error.message}</p>}
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
      )}
    </Section>
  )
}

/** The regions Paperboat serves your account from: one row per place, with how many servers are up. */
function PaperboatServers() {
  const edges = useApiQuery(edgesQuery)
  if (!edges.data?.length) return null
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
        <div key={region} className="flex min-h-11 items-center gap-3 border-b border-divider text-sm">
          <span className="min-w-0 flex-1 truncate">
            {placeName(region)}
            {place.total > 1 && <span className="text-muted-foreground"> · {place.total} servers</span>}
          </span>
          <span className="flex items-center gap-2 text-muted-foreground">
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
            <p className="text-xs text-muted-foreground">It’s used once you choose it below.</p>
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
