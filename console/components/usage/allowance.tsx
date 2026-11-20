"use client"

import { trafficBreakdown, usageSinceQuery, useApiQuery } from "@/lib/api"
import { usageWindowStart, type BillingWorkspace, type fairUse } from "@/lib/billing"
import { formatBytes } from "@/lib/format"
import { cn } from "@/lib/utils"
import { Skeleton } from "@/components/ui/skeleton"

type Fair = NonNullable<ReturnType<typeof fairUse>>

/** The hosted allowance's two kinds of traffic, in the bandwidth card's colours. */
const kinds = [
  { key: "relay", label: "Relay", dot: "bg-path-relay" },
  { key: "edge", label: "Edge", dot: "bg-path-edge" },
] as const

/** This period's traffic through Paperboat's hosted relays and edges, from the start the allowance counts from. */
export function useHostedSplit(billing: BillingWorkspace | undefined, workspace: string) {
  const usage = useApiQuery(usageSinceQuery(workspace, billing ? usageWindowStart(billing) : ""))
  return usage.data && trafficBreakdown(usage.data.totals.by_path, "managed").byPath
}
type Split = ReturnType<typeof useHostedSplit>

/** What is left of the one pool; once it is used up, the slowed state (nothing is cut off) in its place. */
export function AllowanceHeadline({ fair, until, speed }: { fair: Fair; until?: string; speed: string }) {
  return (
    <span className="flex flex-wrap items-baseline gap-x-2">
      <span className="shrink-0 font-heading text-3xl font-semibold tracking-tight whitespace-nowrap">
        {fair.throttled ? "Slowed" : formatBytes(Math.max(0, fair.amount - fair.used))}
      </span>
      <span className="text-sm text-muted-foreground" data-fair-use-state>
        {fair.throttled ? `to ${speed}${until ? ` until ${until}` : ""}` : "left"}
      </span>
    </span>
  )
}

/** The used part of the pool, split by relay and edge; one colour while the split is unknown. */
export function AllowanceBar({ fair, split }: { fair: Fair; split: Split }) {
  const hosted = split ? split.relay + split.edge : 0
  return (
    <div
      role="meter"
      aria-label="Hosted allowance used"
      aria-valuemin={0}
      aria-valuemax={fair.amount}
      aria-valuenow={Math.min(fair.used, fair.amount)}
      className="flex h-1.5 overflow-hidden rounded-full bg-muted"
    >
      {split && hosted > 0 ? (
        kinds.map(({ key, label, dot }) => (
          <div
            key={key}
            title={`${label} ${formatBytes(split[key])}`}
            className={dot}
            style={{ width: `${(fair.share * 100 * split[key]) / hosted}%` }}
          />
        ))
      ) : (
        <div className="bg-primary" style={{ width: `${fair.share * 100}%` }} />
      )}
    </div>
  )
}

export function AllowanceLegend({ split }: { split: Split }) {
  return (
    <div className="flex items-center gap-4 text-xs whitespace-nowrap text-muted-foreground">
      {kinds.map(({ key, label, dot }) => (
        <span key={key} className="flex items-center gap-1.5">
          <span className={cn("size-2 shrink-0 rounded-full", dot)} />
          {label}
          {split ? <span className="text-foreground tabular-nums">{formatBytes(split[key])}</span> : <Skeleton className="h-2.5 w-10" />}
        </span>
      ))}
    </div>
  )
}
