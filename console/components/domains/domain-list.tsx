"use client"

import {
  CheckCircleIcon,
  CircleNotchIcon,
  PlusIcon,
  StarIcon,
  TrashIcon,
  WarningCircleIcon,
} from "@phosphor-icons/react"
import { useMutation, useQueries, useQueryClient } from "@tanstack/react-query"
import Link from "next/link"
import { useDeferredValue, useState } from "react"
import { toast } from "sonner"
import { useApiQuery, useWorkspace } from "@/lib/api"
import {
  accountDomainsQuery,
  addAccountDomain,
  cleanDomain,
  domainPattern,
  isUnder,
  removeAccountDomain,
  verifyAccountDomain,
  type AccountDomain,
} from "@/lib/domains"
import {
  addDomain as addMachineDomain,
  domainCheckQuery,
  gatewayAddress,
  recordName,
  removeDomain as removeMachineDomain,
  setPrimaryDomain,
  useMachineDomains,
} from "@/lib/machine-domains"
import {
  domainsQuery,
  hostOf,
  previewsQuery,
  tunnelsQuery,
  type Preview,
  type Tunnel,
  type TunnelDomain,
} from "@/lib/tunnels"
import { cn } from "@/lib/utils"
import { CopyText } from "@/components/copy-button"
import { ConfirmAction, SearchField } from "@/components/list-toolbar"
import { RowMenus, type Entry } from "@/components/row-menus"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldError, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Sheet, SheetContent, SheetHeader, SheetTitle } from "@/components/ui/sheet"
import { Switch } from "@/components/ui/switch"

/*
 * Your domains, like Vercel's or Cloudflare's: add one once, prove you own it with a TXT record,
 * then use names under it for machine addresses, tunnel public hostnames and previews. The list
 * says what each domain is used for; its panel holds the records, the machine-address switch and
 * everything using it.
 */

type Tone = "ok" | "wait" | "bad"

function State({ tone, label, busy }: { tone: Tone; label: string; busy?: boolean }) {
  return (
    <span
      className={cn(
        "flex items-center gap-1.5 text-sm whitespace-nowrap",
        tone === "ok" ? "text-success" : tone === "bad" ? "text-destructive" : "text-muted-foreground",
      )}
    >
      {busy ? (
        <CircleNotchIcon className="size-3.5 animate-spin" />
      ) : tone === "ok" ? (
        <CheckCircleIcon weight="fill" className="size-3.5" />
      ) : tone === "bad" ? (
        <WarningCircleIcon weight="fill" className="size-3.5" />
      ) : null}
      <span className="trim-cap">{label}</span>
    </span>
  )
}

/** One domain with everything that uses it. */
type Entryish = {
  name: string
  account?: AccountDomain
  /** Paperboat's own machine domain, which can't be removed. */
  managed: boolean
  machines: boolean
  primary: boolean
  hostnames: { domain: TunnelDomain; tunnel: Tunnel }[]
  previews: { hostname: string; preview: Preview }[]
}

const plural = (count: number, one: string, many: string) => `${count} ${count === 1 ? one : many}`

function usedFor(entry: Entryish) {
  const uses = [
    entry.machines && "Machine addresses",
    entry.hostnames.length > 0 && plural(entry.hostnames.length, "tunnel hostname", "tunnel hostnames"),
    entry.previews.length > 0 && plural(entry.previews.length, "preview", "previews"),
  ].filter(Boolean)
  return uses.length ? uses.join(", ") : "Not used yet"
}

function ownership(entry: Entryish): { tone: Tone; label: string } {
  if (entry.managed) return { tone: "ok", label: "Paperboat’s" }
  if (!entry.account) return { tone: "wait", label: "Not added" }
  if (entry.account.state === "verified") return { tone: "ok", label: "Verified" }
  if (entry.account.state === "failed") return { tone: "bad", label: "Verification failed" }
  return { tone: "wait", label: "Waiting for verification" }
}

export function DomainList() {
  const workspace = useWorkspace()
  const ws = workspace?.id ?? ""
  const account = useApiQuery({ ...accountDomainsQuery(ws), enabled: Boolean(workspace) })
  const machineDomains = useMachineDomains()
  const tunnels = useApiQuery({ ...tunnelsQuery(ws), enabled: Boolean(workspace) })
  const bindings = useQueries({ queries: (tunnels.data ?? []).map((tunnel) => domainsQuery(tunnel.id)) })
  const previews = useApiQuery({ ...previewsQuery(ws), enabled: Boolean(workspace) })
  const [search, setSearch] = useState("")
  const text = useDeferredValue(search.trim().toLowerCase())
  const [adding, setAdding] = useState(false)
  const [open, setOpen] = useState<string>()

  const hostnames = (tunnels.data ?? []).flatMap((tunnel, index) =>
    (bindings[index]?.data ?? []).map((domain) => ({ domain, tunnel })),
  )
  const previewNames = (previews.data ?? [])
    .filter((preview) => preview.state !== "expired" && preview.state !== "stopped")
    .flatMap((preview) => preview.domains.map((domain) => ({ hostname: domain.hostname, preview })))

  // Your domains, plus machine domains kept in this browser until the server stores them.
  const names = [
    ...new Set([
      ...machineDomains.domains.filter((domain) => domain.managed).map((domain) => domain.name),
      ...(account.data ?? []).map((domain) => domain.name),
      ...machineDomains.domains.filter((domain) => !domain.managed).map((domain) => domain.name),
    ]),
  ]
  const entries: Entryish[] = names.map((name) => ({
    name,
    account: account.data?.find((domain) => domain.name === name),
    managed: machineDomains.domains.some((domain) => domain.name === name && domain.managed),
    machines: machineDomains.domains.some((domain) => domain.name === name),
    primary: machineDomains.primary === name,
    hostnames: hostnames.filter(({ domain }) => isUnder(domain.hostname, name)),
    previews: previewNames.filter(({ hostname }) => isUnder(hostname, name)),
  }))
  const shown = entries.filter((entry) => !text || entry.name.includes(text))
  const selected = entries.find((entry) => entry.name === open)

  return (
    <section className="flex flex-col gap-3">
      <div className="flex items-center gap-2">
        <SearchField value={search} onChange={setSearch} placeholder="Search domains" />
        <div className="ml-auto">
          <Button size="lg" onClick={() => setAdding(true)}>
            <PlusIcon data-icon="inline-start" />
            Add domain
          </Button>
        </div>
      </div>
      {account.isError && (
        <p className="text-sm text-muted-foreground">Couldn’t load your domains. {account.error.message}</p>
      )}

      <table className="w-full table-fixed text-sm">
        <colgroup>
          <col className="w-[34%]" />
          <col />
          <col className="w-[24%]" />
          <col className="w-12" />
        </colgroup>
        <thead className="border-b border-divider text-left text-xs text-muted-foreground">
          <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
            <th>Domain</th>
            <th>Used for</th>
            <th>Status</th>
            <th aria-label="Actions" />
          </tr>
        </thead>
        <tbody className="divide-y divide-divider border-b border-divider">
          {shown.map((entry) => (
            <DomainRow key={entry.name} entry={entry} onOpen={() => setOpen(entry.name)} />
          ))}
        </tbody>
      </table>
      {text && !shown.length && (
        <p className="py-12 text-center text-sm text-muted-foreground">No domains match “{search.trim()}”</p>
      )}

      {adding && (
        <AddDomainDialog
          existing={names}
          onClose={() => setAdding(false)}
          onAdded={(name) => {
            setAdding(false)
            setOpen(name)
          }}
        />
      )}
      <Sheet open={Boolean(selected)} onOpenChange={(next) => !next && setOpen(undefined)}>
        <SheetContent className="w-full gap-0 sm:max-w-lg">
          {selected && <DomainPanel entry={selected} onRemoved={() => setOpen(undefined)} />}
        </SheetContent>
      </Sheet>
    </section>
  )
}

function DomainRow({ entry, onOpen }: { entry: Entryish; onOpen: () => void }) {
  const [removing, setRemoving] = useState(false)
  const state = ownership(entry)
  const menu: Entry[] = [
    ...(entry.machines
      ? ([
          {
            kind: "item",
            label: "Make default for machines",
            icon: StarIcon,
            disabled: entry.primary,
            onSelect: () => setPrimaryDomain(entry.name),
          },
        ] satisfies Entry[])
      : []),
    ...(entry.managed
      ? []
      : ([
          ...(entry.machines ? ([{ kind: "separator" }] satisfies Entry[]) : []),
          { kind: "item", label: "Remove", icon: TrashIcon, destructive: true, onSelect: () => setRemoving(true) },
        ] satisfies Entry[])),
  ]
  const row = (actions: React.ReactNode) => (
    <tr className="group relative transition-colors hover:bg-surface data-popup-open:bg-surface [&>td]:h-14 [&>td]:px-3">
      <td>
        <span className="flex h-[1lh] min-w-0 items-center gap-2">
          <button
            type="button"
            onClick={onOpen}
            className="min-w-0 truncate-x text-left font-mono text-code font-medium outline-none trim-cap after:absolute after:inset-0"
          >
            {entry.name}
          </button>
          {entry.primary && (
            <Badge variant="outline" className="font-normal text-muted-foreground trim-cap">
              Default
            </Badge>
          )}
        </span>
      </td>
      <td className="truncate text-muted-foreground">{usedFor(entry)}</td>
      <td>
        <State tone={state.tone} label={state.label} />
      </td>
      <td className="text-right">{actions}</td>
    </tr>
  )
  if (!menu.some((item) => item.kind === "item" && !item.disabled)) return row(null)
  return (
    <RowMenus
      label={`Actions for ${entry.name}`}
      entries={menu}
      row={row}
      dialogs={<RemoveDomain entry={entry} open={removing} onOpenChange={setRemoving} />}
    />
  )
}

function RemoveDomain({
  entry,
  open,
  onOpenChange,
  onRemoved,
}: {
  entry: Entryish
  open: boolean
  onOpenChange: (open: boolean) => void
  onRemoved?: () => void
}) {
  const queryClient = useQueryClient()
  const inUse = entry.hostnames.length + entry.previews.length
  return (
    <ConfirmAction
      open={open}
      onOpenChange={onOpenChange}
      title={`Remove ${entry.name}?`}
      description={
        inUse
          ? `${plural(inUse, "hostname", "hostnames")} under it stop working, and machines stop answering on it. DNS records stay at your DNS provider.`
          : "Machines stop answering on it, and it can’t be used for tunnels or previews until you add it again."
      }
      action="Remove domain"
      pending="Removing…"
      confirmName={inUse ? entry.name : undefined}
      mutation={{
        mutationFn: async () => {
          if (entry.account) await removeAccountDomain(entry.account)
          if (entry.machines) removeMachineDomain(entry.name)
        },
        onSuccess: async () => {
          await queryClient.invalidateQueries({ queryKey: ["domains"] })
          toast.success(`${entry.name} removed`)
          onRemoved?.()
        },
      }}
    />
  )
}

/** A domain's panel: proof of ownership, machine addresses, and what uses it. */
function DomainPanel({ entry, onRemoved }: { entry: Entryish; onRemoved: () => void }) {
  const queryClient = useQueryClient()
  const [removing, setRemoving] = useState(false)
  const verify = useMutation({
    mutationFn: () => verifyAccountDomain(entry.account!),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["domains"] }),
    onError: (error) => toast.error("Couldn’t check the record", { description: error.message }),
  })
  const state = ownership(entry)
  return (
    <>
      <SheetHeader className="gap-1 border-b border-divider p-6">
        <SheetTitle className="font-mono text-base">{entry.name}</SheetTitle>
        <State tone={state.tone} label={state.label} />
      </SheetHeader>
      <div className="flex flex-col gap-8 overflow-y-auto p-6">
        {entry.account && entry.account.state !== "verified" && entry.account.verification && (
          <PanelSection title="Prove you own it">
            <p className="text-sm text-muted-foreground">Add this record at your DNS provider, then check.</p>
            <Record
              type={entry.account.verification.type}
              name={entry.account.verification.name}
              value={entry.account.verification.value}
            />
            <Button variant="secondary" className="w-fit" onClick={() => verify.mutate()} disabled={verify.isPending}>
              {verify.isPending ? "Checking…" : "Check again"}
            </Button>
          </PanelSection>
        )}

        <MachineAddresses entry={entry} />

        <PanelSection title="Tunnels">
          {entry.hostnames.length ? (
            <ul className="flex flex-col divide-y divide-divider border-y border-divider">
              {entry.hostnames.map(({ domain, tunnel }) => (
                <li key={domain.id} className="flex min-h-11 items-center gap-3 text-sm">
                  <span className="min-w-0 flex-1 truncate font-mono text-code">{domain.hostname}</span>
                  <Link
                    href={`/tunnels/${encodeURIComponent(tunnel.id)}`}
                    className="shrink-0 text-muted-foreground underline-offset-4 hover:text-foreground hover:underline"
                  >
                    {tunnel.name}
                  </Link>
                </li>
              ))}
            </ul>
          ) : (
            <p className="text-sm text-muted-foreground">
              Use names like app.{entry.name} or *.{entry.name} from a tunnel’s Public hostnames.
            </p>
          )}
        </PanelSection>

        <PanelSection title="Previews">
          {entry.previews.length ? (
            <ul className="flex flex-col divide-y divide-divider border-y border-divider">
              {entry.previews.map(({ hostname, preview }) => (
                <li key={hostname} className="flex min-h-11 items-center gap-3 text-sm">
                  <a
                    href={`https://${hostname}`}
                    target="_blank"
                    rel="noopener"
                    className="min-w-0 flex-1 truncate font-mono text-code underline-offset-4 hover:underline"
                  >
                    {hostname}
                  </a>
                  <span className="shrink-0 font-mono text-code text-muted-foreground">
                    {hostOf(preview.endpoint).split(".")[0]}
                  </span>
                </li>
              ))}
            </ul>
          ) : (
            <p className="text-sm text-muted-foreground">New previews can use addresses like demo.{entry.name}.</p>
          )}
        </PanelSection>

        {!entry.managed && (
          <div className="flex flex-wrap items-center gap-4 rounded-lg bg-destructive/5 p-4 dark:bg-destructive/10">
            <div className="flex min-w-0 flex-1 flex-col gap-0.5">
              <p className="text-sm font-medium">Remove domain</p>
              <p className="text-sm text-muted-foreground">Everything using it stops working.</p>
            </div>
            <Button
              variant="secondary"
              className="bg-background text-destructive hover:bg-background/70 dark:bg-white/10 dark:hover:bg-white/15"
              onClick={() => setRemoving(true)}
            >
              Remove
            </Button>
          </div>
        )}
      </div>
      <RemoveDomain entry={entry} open={removing} onOpenChange={setRemoving} onRemoved={onRemoved} />
    </>
  )
}

/** Machine addresses on this domain (`homelab.<domain>`): a switch, and its DNS record until it works. */
function MachineAddresses({ entry }: { entry: Entryish }) {
  const check = useApiQuery({ ...domainCheckQuery(entry.name), enabled: entry.machines })
  const ready = check.data?.status === "ready"
  return (
    <PanelSection
      title="Machine addresses"
      action={
        !entry.managed && (
          <Switch
            aria-label="Use for machine addresses"
            checked={entry.machines}
            onCheckedChange={(on) => (on ? addMachineDomain(entry.name) : removeMachineDomain(entry.name))}
          />
        )
      }
    >
      <p className="text-sm text-muted-foreground">
        {entry.machines ? (
          <>
            Machines answer as <span className="font-mono text-code text-foreground">homelab.{entry.name}</span> from
            any computer running Paperboat.
          </>
        ) : (
          <>Give every machine an address under {entry.name}, reachable from any computer running Paperboat.</>
        )}
      </p>
      {entry.machines && !entry.managed && !check.isPending && !ready && (
        <>
          <Record type="A" name={recordName(entry.name, check.data?.zone)} value={gatewayAddress} />
          <div className="flex items-center gap-3 text-xs text-muted-foreground">
            {check.data?.status === "wrong_address" && <span>Now {check.data.addresses.join(", ")}.</span>}
            <button
              type="button"
              onClick={() => check.refetch()}
              disabled={check.isFetching}
              className="underline-offset-4 hover:text-foreground hover:underline"
            >
              Check again
            </button>
          </div>
        </>
      )}
      {entry.machines && !entry.primary && (
        <Button variant="secondary" className="w-fit" disabled={!ready} onClick={() => setPrimaryDomain(entry.name)}>
          Make default for machines
        </Button>
      )}
    </PanelSection>
  )
}

function PanelSection({
  title,
  action,
  children,
}: {
  title: string
  action?: React.ReactNode
  children: React.ReactNode
}) {
  return (
    <section className="flex flex-col gap-3">
      <div className="flex min-h-6 items-center justify-between gap-3">
        <h3 className="text-sm font-medium">{title}</h3>
        {action}
      </div>
      {children}
    </section>
  )
}

/** One DNS record to add; each part copies on click. */
function Record({ type, name, value }: { type: string; name: string; value: string }) {
  return (
    <dl className="grid grid-cols-[4rem_minmax(0,1fr)] gap-x-3 gap-y-1.5 rounded-lg bg-surface p-3 text-sm">
      <dt className="text-muted-foreground">Type</dt>
      <dd className="font-mono text-code">{type}</dd>
      <dt className="text-muted-foreground">Name</dt>
      <dd className="min-w-0">
        <CopyText value={name} className="font-mono text-code" />
      </dd>
      <dt className="text-muted-foreground">Value</dt>
      <dd className="min-w-0">
        <CopyText value={value} className="font-mono text-code" />
      </dd>
    </dl>
  )
}

/** Adds a domain; its panel opens next with the record that proves you own it. */
function AddDomainDialog({
  existing,
  onClose,
  onAdded,
}: {
  existing: string[]
  onClose: () => void
  onAdded: (name: string) => void
}) {
  const queryClient = useQueryClient()
  const workspace = useWorkspace()
  const [draft, setDraft] = useState("")
  const name = cleanDomain(draft)
  const problem = !name
    ? undefined
    : !domainPattern.test(name)
      ? "Enter a domain like acme.dev."
      : existing.includes(name)
        ? "That domain is already added."
        : undefined
  const add = useMutation({
    mutationFn: () => addAccountDomain(workspace?.id ?? "personal", name),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["domains"] })
      onAdded(name)
    },
  })
  return (
    <Dialog open onOpenChange={(next) => !next && !add.isPending && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Add a domain</DialogTitle>
        </DialogHeader>
        <form
          className="flex flex-col gap-4"
          onSubmit={(event) => {
            event.preventDefault()
            if (name && !problem) add.mutate()
          }}
        >
          <Field data-invalid={Boolean(problem)} className="gap-1.5">
            <FieldLabel htmlFor="domain-name">Domain</FieldLabel>
            <Input
              id="domain-name"
              autoFocus
              placeholder="acme.dev"
              value={draft}
              onChange={(event) => setDraft(event.target.value)}
              aria-invalid={Boolean(problem)}
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
            />
            {problem ? (
              <FieldError>{problem}</FieldError>
            ) : (
              <p className="text-xs text-muted-foreground">
                Then use names under it for tunnels, previews and machine addresses.
              </p>
            )}
          </Field>
          {add.isError && <p className="text-sm text-destructive">{add.error.message}</p>}
          <DialogFooter>
            <Button type="button" variant="secondary" size="lg" onClick={onClose} disabled={add.isPending}>
              Cancel
            </Button>
            <Button type="submit" size="lg" disabled={!name || Boolean(problem) || add.isPending}>
              {add.isPending ? "Adding…" : "Add domain"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
