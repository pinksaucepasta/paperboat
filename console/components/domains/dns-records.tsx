"use client"

import { CheckCircleIcon, CircleNotchIcon, WarningCircleIcon, WarningIcon } from "@phosphor-icons/react"
import Link from "next/link"
import { CommandBlock } from "@/components/add-machine-dialog"
import { useApiQuery, useWorkspace } from "@/lib/api"
import { cn } from "@/lib/utils"
import { workspaceDomainsQuery, type WorkspaceDomain } from "@/lib/workspace-domains"

type DomainRecord = WorkspaceDomain["records"][number]
type RecordStatus = DomainRecord["record_status"]

const purposes: Record<string, string> = {
  verification: "Proves you control the domain",
  routing: "Needed to serve names under it",
  apex_routing: "Serves the domain itself",
}

const pills: Record<Exclude<RecordStatus, "not_checked">, { label: string; tone: string; icon: React.ReactNode }> = {
  verified: {
    label: "Verified",
    tone: "bg-success/10 text-success",
    icon: <CheckCircleIcon weight="fill" />,
  },
  wrong_address: {
    label: "Wrong address",
    tone: "bg-destructive/10 text-destructive",
    icon: <WarningCircleIcon weight="fill" />,
  },
  missing: {
    label: "Missing",
    tone: "bg-amber-500/15 text-amber-700",
    icon: <WarningIcon weight="fill" />,
  },
  pending: {
    label: "Checking",
    tone: "bg-muted text-muted-foreground",
    icon: <CircleNotchIcon className="animate-spin" />,
  },
}

function Pill({ status, label }: { status: keyof typeof pills; label?: string }) {
  const pill = pills[status]
  return (
    <span
      data-dns-status={status}
      className={cn(
        "inline-flex h-5 shrink-0 items-center gap-1 rounded-full px-2 text-xs font-medium whitespace-nowrap [&>svg]:size-3",
        pill.tone,
      )}
    >
      {pill.icon}
      {label ?? pill.label}
    </span>
  )
}

/** The header summary for a domain's DNS: Healthy only when every checked record is verified. */
export function DnsHealth({ health }: { health: WorkspaceDomain["dns_health"] }) {
  const status = {
    healthy: "verified",
    wrong_address: "wrong_address",
    missing: "missing",
    checking: "pending",
  } as const
  return (
    <span data-dns-health={health}>
      <Pill status={status[health]} label={health === "healthy" ? "Healthy" : undefined} />
    </span>
  )
}

/**
 * The DNS badge of a tunnel's or preview's own domain: its entry in the Domains list (the same name, or the entry it sits
 * under: the longest one), so the status is the list's. Without an entry it says to add the domain there.
 */
export function BindingDns({ hostname }: { hostname: string }) {
  const workspace = useWorkspace()
  const list = useApiQuery({ ...workspaceDomainsQuery(workspace?.id ?? ""), enabled: Boolean(workspace) })
  if (!list.data) return null
  const name = hostname.replace(/^\*\./, "")
  const entry = list.data
    .filter((domain) => name === domain.hostname || name.endsWith(`.${domain.hostname}`))
    .sort((a, b) => b.hostname.length - a.hostname.length)[0]
  return (
    <Link
      href="/network#domains"
      aria-label={entry ? `DNS records of ${entry.hostname}` : `Add ${name} on the Network page`}
      className="rounded-full text-xs outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
    >
      {entry ? (
        <DnsHealth health={entry.dns_health} />
      ) : (
        <span data-dns-not-added className="text-muted-foreground underline-offset-2 hover:underline">
          Not added — add it on the Network page
        </span>
      )}
    </Link>
  )
}

/** What the user must do about a record, in a sentence. Nothing for a verified or pending one. */
function problem(record: DomainRecord, all: DomainRecord[]): string | undefined {
  if (record.purpose === "verification") return undefined // the domain's dns_failure message already says it
  if (record.record_status === "missing")
    return "Not in DNS yet. Names still work through your other records, but without this one as a backup."
  if (record.record_status !== "wrong_address") return undefined
  // Offending values are those DNS returns that none of the expected records for this name has.
  const expected = new Set(all.filter((other) => other.name === record.name).map((other) => other.value))
  const stray = record.observed_values.filter((value) => !expected.has(value))
  const values = stray.length ? stray : record.observed_values
  return `DNS returns ${values.join(", ")} for this name, which ${values.length === 1 ? "is not one of your region machines" : "are not your region machines"}. Visitors sent there fail: remove ${values.length === 1 ? "it" : "them"} at your DNS provider.`
}

/** One expected DNS record with its status. Name and value stay whole on one line and copy on click. */
function Record({ record, all }: { record: DomainRecord; all: DomainRecord[] }) {
  const note = problem(record, all)
  return (
    <div className="flex flex-col gap-1.5" data-dns-record={record.purpose}>
      <div className="flex min-h-5 items-center justify-between gap-3">
        <p className="text-xs text-muted-foreground">{purposes[record.purpose] ?? record.purpose}</p>
        {record.record_status !== "not_checked" && <Pill status={record.record_status} />}
      </div>
      <dl className="grid grid-cols-[3rem_minmax(0,1fr)] items-center gap-x-3 gap-y-1.5 text-sm">
        <dt className="text-muted-foreground">Type</dt>
        <dd className="font-mono text-code">{record.type}</dd>
        <dt className="text-muted-foreground">Name</dt>
        <dd className="min-w-0">
          <CommandBlock command={record.name} label="Copy name" />
        </dd>
        <dt className="text-muted-foreground">Value</dt>
        <dd className="min-w-0">
          <CommandBlock command={record.value} label="Copy value" />
        </dd>
      </dl>
      {note && (
        <p
          data-dns-note
          className={cn(
            "text-sm",
            record.record_status === "wrong_address" ? "text-destructive" : "text-muted-foreground",
          )}
        >
          {note}
        </p>
      )}
    </div>
  )
}

/** The expected records. The root-domain record is optional: it shows only once something uses the root domain. */
export function DnsRecords({ domain }: { domain: WorkspaceDomain }) {
  return domain.records
    .filter((record) => record.record_status !== "not_checked")
    .map((record) => (
      <Record key={`${record.name}-${record.type}-${record.value}`} record={record} all={domain.records} />
    ))
}
