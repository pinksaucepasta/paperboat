"use client"

import { CheckCircleIcon, CircleNotchIcon, PlusIcon, TrashIcon, WarningCircleIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import Link from "next/link"
import { useState } from "react"
import { toast } from "sonner"
import { clientConfigQuery, useApiQuery, useWorkspace } from "@/lib/api"
import { cleanDomain, domainPattern } from "@/lib/domains"
import {
  addWorkspaceDomain,
  checkWorkspaceDomain,
  domainStatus,
  removeWorkspaceDomain,
  workspaceDomainsQuery,
  type Tone,
  type WorkspaceDomain,
} from "@/lib/workspace-domains"
import { cn } from "@/lib/utils"
import { CopyText } from "@/components/copy-button"
import { ConfirmAction } from "@/components/list-toolbar"
import { RowMenus, type Entry } from "@/components/row-menus"
import { Time } from "@/components/time"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldError, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Sheet, SheetContent, SheetHeader, SheetTitle } from "@/components/ui/sheet"
import { Skeleton } from "@/components/ui/skeleton"

/*
 * Domains you add to the workspace once (ngrok reserved domains, Vercel team domains): verified by
 * one _acme-challenge CNAME, then any name under them serves tunnels and previews with HTTPS from
 * the domain's certificate. Paperboat's own machine domain is listed first; it gives machine
 * addresses only.
 */

function State({ tone, label }: { tone: Tone; label: string }) {
  return (
    <span
      className={cn(
        "flex items-center gap-1.5 text-sm whitespace-nowrap",
        tone === "ok" ? "text-success" : tone === "bad" ? "text-destructive" : "text-muted-foreground",
      )}
    >
      {tone === "ok" ? (
        <CheckCircleIcon weight="fill" className="size-3.5" />
      ) : tone === "bad" ? (
        <WarningCircleIcon weight="fill" className="size-3.5" />
      ) : (
        <CircleNotchIcon className="size-3.5 animate-spin" />
      )}
      <span className="trim-cap">{label}</span>
    </span>
  )
}

const plural = (count: number, one: string, many: string) => `${count} ${count === 1 ? one : many}`

function usedBy(domain: WorkspaceDomain) {
  const tunnels = domain.used_by.filter((use) => use.kind === "tunnel").length
  const previews = domain.used_by.length - tunnels
  const uses = [
    tunnels && plural(tunnels, "tunnel hostname", "tunnel hostnames"),
    previews && plural(previews, "preview", "previews"),
  ]
  return uses.filter(Boolean).join(", ") || "Not used yet"
}

export function DomainList() {
  const workspace = useWorkspace()
  const ws = workspace?.id ?? ""
  const list = useApiQuery({ ...workspaceDomainsQuery(ws), enabled: Boolean(workspace) })
  const machineDomain = useApiQuery(clientConfigQuery).data?.local_access_base_domain
  const [adding, setAdding] = useState(false)
  const [open, setOpen] = useState<string>()
  const selected = list.data?.find((domain) => domain.id === open)

  return (
    <section className="flex flex-col gap-3">
      <div className="flex items-center gap-2">
        <div className="ml-auto">
          <Button size="lg" onClick={() => setAdding(true)}>
            <PlusIcon data-icon="inline-start" />
            Add domain
          </Button>
        </div>
      </div>

      <table className="w-full table-fixed text-sm">
        <colgroup>
          <col className="w-[38%]" />
          <col />
          <col className="w-[24%]" />
          <col className="w-12" />
        </colgroup>
        <thead className="border-b border-divider text-left text-xs text-muted-foreground">
          <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
            <th>Domain</th>
            <th>Used by</th>
            <th>Status</th>
            <th aria-label="Actions" />
          </tr>
        </thead>
        <tbody className="divide-y divide-divider border-b border-divider">
          {machineDomain && (
            <tr className="[&>td]:h-14 [&>td]:px-3">
              <td className="truncate font-mono text-code font-medium">{machineDomain}</td>
              <td className="truncate text-muted-foreground">
                Machine addresses, like <span className="font-mono text-code">homelab.{machineDomain}</span>
              </td>
              <td>
                <State tone="ok" label="Ready" />
              </td>
              <td />
            </tr>
          )}
          {list.data?.map((domain) => (
            <DomainRow key={domain.id} domain={domain} onOpen={() => setOpen(domain.id)} />
          ))}
        </tbody>
      </table>
      {list.isPending && <Skeleton className="h-4 w-1/3" />}
      {list.isError && (
        <p className="py-3 text-sm text-muted-foreground">Couldn’t load domains. {list.error.message}</p>
      )}
      {list.data?.length === 0 && (
        <div className="flex flex-col items-center gap-3 py-12 text-center">
          <p className="text-sm text-muted-foreground">
            Add a domain once, then use any name under it for your tunnels and previews.
          </p>
          <Button variant="secondary" onClick={() => setAdding(true)}>
            <PlusIcon data-icon="inline-start" />
            Add domain
          </Button>
        </div>
      )}

      {adding && (
        <AddDomainDialog
          onClose={() => setAdding(false)}
          onAdded={(id) => {
            setAdding(false)
            setOpen(id)
          }}
        />
      )}
      <Sheet open={Boolean(selected)} onOpenChange={(next) => !next && setOpen(undefined)}>
        <SheetContent className="w-full gap-0 sm:max-w-lg">
          {selected && <DomainPanel domain={selected} onRemoved={() => setOpen(undefined)} />}
        </SheetContent>
      </Sheet>
    </section>
  )
}

function DomainRow({ domain, onOpen }: { domain: WorkspaceDomain; onOpen: () => void }) {
  const [removing, setRemoving] = useState(false)
  const status = domainStatus(domain)
  const menu: Entry[] = [
    {
      kind: "item",
      label: domain.used_by.length ? "Remove (in use)" : "Remove",
      icon: TrashIcon,
      destructive: true,
      disabled: domain.used_by.length > 0,
      onSelect: () => setRemoving(true),
    },
  ]
  return (
    <RowMenus
      label={`Actions for ${domain.hostname}`}
      entries={menu}
      row={(actions) => (
        <tr className="group relative transition-colors hover:bg-surface data-popup-open:bg-surface [&>td]:h-14 [&>td]:px-3">
          <td>
            <button
              type="button"
              onClick={onOpen}
              className="max-w-full truncate-x text-left font-mono text-code font-medium outline-none trim-cap after:absolute after:inset-0"
            >
              {domain.hostname}
            </button>
          </td>
          <td className="truncate text-muted-foreground">{usedBy(domain)}</td>
          <td>
            <State tone={status.tone} label={status.label} />
          </td>
          <td className="text-right">{actions}</td>
        </tr>
      )}
      dialogs={<RemoveDomain domain={domain} open={removing} onOpenChange={setRemoving} />}
    />
  )
}

function RemoveDomain({
  domain,
  open,
  onOpenChange,
  onRemoved,
}: {
  domain: WorkspaceDomain
  open: boolean
  onOpenChange: (open: boolean) => void
  onRemoved?: () => void
}) {
  const workspace = useWorkspace()
  const queryClient = useQueryClient()
  return (
    <ConfirmAction
      open={open}
      onOpenChange={onOpenChange}
      title={`Remove ${domain.hostname}?`}
      description="Tunnels and previews can no longer use names under it. Your DNS records stay at your provider."
      action="Remove domain"
      pending="Removing…"
      mutation={{
        mutationFn: () => removeWorkspaceDomain(workspace?.id ?? "", domain.id),
        onSuccess: async () => {
          await queryClient.invalidateQueries({ queryKey: ["workspace-domains"] })
          toast.success(`${domain.hostname} removed`)
          onRemoved?.()
        },
      }}
    />
  )
}

const purposes: Record<string, string> = {
  verification: "Proves you control the domain",
  routing: "Sends every name under it to Paperboat",
  apex_routing: `Optional: serves the domain itself`,
}

/** A domain's panel: the DNS records with live status, the certificate, and what uses it. */
function DomainPanel({ domain, onRemoved }: { domain: WorkspaceDomain; onRemoved: () => void }) {
  const workspace = useWorkspace()
  const queryClient = useQueryClient()
  const [removing, setRemoving] = useState(false)
  const status = domainStatus(domain)
  const check = useMutation({
    mutationFn: () => checkWorkspaceDomain(workspace?.id ?? "", domain.id),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["workspace-domains"] }),
    onError: (error) => toast.error("Couldn’t check DNS", { description: error.message }),
  })
  const verified = domain.state === "verified"
  return (
    <>
      <SheetHeader className="gap-1 border-b border-divider p-6">
        <SheetTitle className="font-mono text-base">{domain.hostname}</SheetTitle>
        <State tone={status.tone} label={status.label} />
      </SheetHeader>
      <div className="flex flex-col gap-8 overflow-y-auto p-6">
        <PanelSection
          title="DNS records"
          action={
            <Button variant="secondary" size="sm" disabled={check.isPending} onClick={() => check.mutate()}>
              {check.isPending ? "Checking…" : "Check now"}
            </Button>
          }
        >
          {!verified && (
            <p className="text-sm text-muted-foreground">
              Add these at your DNS provider. Paperboat checks them on its own and this page updates.
            </p>
          )}
          {domain.records.map((record) => (
            <Record key={`${record.name}-${record.type}-${record.value}`} record={record} />
          ))}
          {domain.dns_failure && <p className="text-sm text-destructive">{domain.dns_failure.message}</p>}
          {domain.last_checked_at && (
            <p className="text-xs text-muted-foreground">
              Checked <Time iso={domain.last_checked_at} relative />
            </p>
          )}
        </PanelSection>

        <PanelSection title="Certificate">
          <p className="text-sm text-muted-foreground">
            {domain.certificate.state === "ready" && domain.certificate.expires_at ? (
              <>
                Covers {domain.hostname} and every name under it until <Time iso={domain.certificate.expires_at} />.
              </>
            ) : domain.certificate.state === "failed" ? (
              <span className="text-destructive">
                {domain.certificate.failure?.message ?? "Couldn’t be issued yet."}
              </span>
            ) : verified ? (
              "Being issued for the domain and every name under it."
            ) : (
              "Issued once the domain is verified."
            )}
          </p>
        </PanelSection>

        <PanelSection title="Used by">
          {domain.used_by.length ? (
            <ul className="flex flex-col divide-y divide-divider border-y border-divider">
              {domain.used_by.map((use) => (
                <li key={`${use.kind}-${use.hostname}`} className="flex min-h-11 items-center gap-3 text-sm">
                  <span className="min-w-0 flex-1 truncate font-mono text-code">{use.hostname}</span>
                  {use.kind === "tunnel" ? (
                    <Link
                      href={`/tunnels/${encodeURIComponent(use.id)}`}
                      className="truncate text-muted-foreground hover:text-foreground"
                    >
                      Tunnel {use.name}
                    </Link>
                  ) : (
                    <Link href="/previews" className="truncate text-muted-foreground hover:text-foreground">
                      Preview
                    </Link>
                  )}
                </li>
              ))}
            </ul>
          ) : (
            <p className="text-sm text-muted-foreground">
              {verified ? (
                <>
                  Use names like <span className="font-mono text-code text-foreground">app.{domain.hostname}</span> in a
                  tunnel’s Public hostnames or a new preview.
                </>
              ) : (
                "Tunnels and previews can use names under it once it’s verified."
              )}
            </p>
          )}
        </PanelSection>

        <div className="flex flex-wrap items-center gap-4 rounded-lg bg-destructive/5 p-4 dark:bg-destructive/10">
          <div className="flex min-w-0 flex-1 flex-col gap-0.5">
            <p className="text-sm font-medium">Remove domain</p>
            <p className="text-sm text-muted-foreground">
              {domain.used_by.length
                ? "Remove the hostnames that use it first."
                : "Your DNS records stay at your provider."}
            </p>
          </div>
          <Button
            variant="secondary"
            disabled={domain.used_by.length > 0}
            className="bg-background text-destructive hover:bg-background/70 dark:bg-white/10 dark:hover:bg-white/15"
            onClick={() => setRemoving(true)}
          >
            Remove
          </Button>
        </div>
      </div>
      <RemoveDomain domain={domain} open={removing} onOpenChange={setRemoving} onRemoved={onRemoved} />
    </>
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
function Record({ record }: { record: WorkspaceDomain["records"][number] }) {
  return (
    <div className="flex flex-col gap-1.5">
      <p className="text-xs text-muted-foreground">{purposes[record.purpose] ?? record.purpose}</p>
      <dl className="grid grid-cols-[4rem_minmax(0,1fr)] gap-x-3 gap-y-1.5 rounded-lg bg-surface p-3 text-sm">
        <dt className="text-muted-foreground">Type</dt>
        <dd className="font-mono text-code">{record.type}</dd>
        <dt className="text-muted-foreground">Name</dt>
        <dd className="min-w-0">
          <CopyText value={record.name} className="font-mono text-code" />
        </dd>
        <dt className="text-muted-foreground">Value</dt>
        <dd className="min-w-0">
          <CopyText value={record.value} className="font-mono text-code" />
        </dd>
      </dl>
    </div>
  )
}

function AddDomainDialog({ onClose, onAdded }: { onClose: () => void; onAdded: (id: string) => void }) {
  const workspace = useWorkspace()
  const queryClient = useQueryClient()
  const [draft, setDraft] = useState("")
  const name = cleanDomain(draft)
  const problem = name && !domainPattern.test(name) ? "Enter a domain like example.com or dev.example.com" : undefined
  const add = useMutation({
    mutationFn: () => addWorkspaceDomain(workspace?.id ?? "", name),
    onSuccess: async (domain) => {
      await queryClient.invalidateQueries({ queryKey: ["workspace-domains"] })
      onAdded(domain.id)
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
          <Field data-invalid={Boolean(problem || add.isError)} className="gap-1.5">
            <FieldLabel htmlFor="domain-name">Domain</FieldLabel>
            <Input
              id="domain-name"
              autoFocus
              placeholder="dev.example.com"
              value={draft}
              onChange={(event) => setDraft(event.target.value)}
              aria-invalid={Boolean(problem)}
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
            />
            {problem || add.isError ? (
              <FieldError>{problem ?? add.error?.message}</FieldError>
            ) : (
              <p className="text-xs text-muted-foreground">
                A domain you control. Next you’ll get the DNS records to add.
              </p>
            )}
          </Field>
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
