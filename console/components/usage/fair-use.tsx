"use client"

import Link from "next/link"
import { billingQuery, fairUse, formatSpeed } from "@/lib/billing"
import { useApiQuery, useWorkspace } from "@/lib/api"
import { formatBytes } from "@/lib/format"
import { AllowanceBar, AllowanceHeadline, AllowanceLegend, useHostedSplit } from "@/components/usage/allowance"
import { useTimeFormat } from "@/lib/use-time-format"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"

/**
 * Traffic through Paperboat's edges and relays (one pool): what is included, what is used of it and, past it, the reduced speed and when full
 * speed returns. Never an error: nothing is cut off. Adding bandwidth or changing plan lifts it at once. A workspace on its own
 * edges is told it has no limits.
 */
export function FairUse() {
  const workspace = useWorkspace()
  const billing = useApiQuery(billingQuery(workspace))
  const time = useTimeFormat()
  const split = useHostedSplit(billing.data, workspace?.id ?? "")
  if (!workspace || billing.isError) return null
  const data = billing.data
  if (!data) return <Skeleton className="h-36 w-full" />
  const fair = fairUse(data)
  if (!fair) return null
  const until = fair.ends ? time.date(fair.ends, "day") : undefined
  const canBuy = workspace.kind === "personal" || workspace.role === "owner" || workspace.role === "admin"
  const nearly = !fair.ownEdges && (fair.throttled || fair.share >= 0.8)
  return (
    <section
      data-fair-use
      data-throttled={fair.throttled}
      data-own-edges={fair.ownEdges}
      aria-label="Hosted allowance"
      className="flex flex-col gap-3 rounded-lg border border-divider bg-surface p-4"
    >
      {fair.ownEdges ? (
        <>
          <span className="text-2xl font-medium tracking-tight">Unlimited (your own edges)</span>
          <p className="text-sm text-muted-foreground">
            Traffic through your own edges and relays is never counted or limited by Paperboat. Previews and tunnels on them have no plan
            limits.
          </p>
        </>
      ) : (
        <>
          <AllowanceHeadline fair={fair} until={until} speed={formatSpeed(fair.mbit)} />
          <div className="flex flex-col gap-2.5">
            <AllowanceBar fair={fair} split={split} />
            <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-0.5 text-sm text-muted-foreground">
              <span className="tabular-nums">{`${formatBytes(fair.used)} of ${formatBytes(fair.amount)}`}</span>
              {until && <span>Resets {until}</span>}
            </div>
            <AllowanceLegend split={split} />
          </div>
          <p className="text-sm text-muted-foreground">
            {formatBytes(fair.amount)} a month of traffic included
            {fair.seats ? ` (${formatBytes(fair.seats.perSeat)} × ${fair.seats.count} seat${fair.seats.count === 1 ? "" : "s"}, shared by the team)` : ""}
            . Edges and relays share it: previews, tunnels and relayed connections count.
            Files you upload through a console terminal count too; typing and terminal output do not, and are never slowed by it. After that, speed is
            reduced to {formatSpeed(fair.mbit)} until your next billing date. Never cut off, never billed extra.
          </p>
          <p className="text-xs text-muted-foreground">
            Until then relayed connections run at up to {formatSpeed(fair.relay.mbit)} and edges at full speed. Direct connections are not
            counted. Run your own edge or relay (free, open source): no limits.
          </p>
        </>
      )}
      {nearly && canBuy && (
        <div className="flex flex-wrap items-center gap-2">
          {!data.granted && (
            <Button size="lg" nativeButton={false} render={<Link href="/billing" />}>
              Add bandwidth
            </Button>
          )}
          <Button size="lg" variant="outline" nativeButton={false} render={<Link href="/billing" />}>
            {data.granted ? "Subscribe" : "Change plan"}
          </Button>
        </div>
      )}
      {nearly && !canBuy && <p className="text-xs text-muted-foreground">Ask an owner or admin to add bandwidth.</p>}
    </section>
  )
}
