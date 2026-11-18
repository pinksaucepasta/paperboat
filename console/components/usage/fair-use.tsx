"use client"

import Link from "next/link"
import { billingQuery, fairUse, formatSpeed } from "@/lib/billing"
import { useApiQuery, useWorkspace } from "@/lib/api"
import { formatBytes } from "@/lib/format"
import { useTimeFormat } from "@/lib/use-time-format"
import { cn } from "@/lib/utils"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"

/**
 * Public traffic through Paperboat's edges: what is included, what is used of it and, past it, the reduced speed and when full
 * speed returns. Never an error: nothing is cut off. Adding bandwidth or changing plan lifts it at once. A workspace on its own
 * edges is told it has no limits.
 */
export function FairUse() {
  const workspace = useWorkspace()
  const billing = useApiQuery(billingQuery(workspace))
  const time = useTimeFormat()
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
      aria-label="Public traffic"
      className="flex flex-col gap-3 rounded-lg border border-divider bg-surface p-4"
    >
      {fair.ownEdges ? (
        <>
          <span className="text-2xl font-medium tracking-tight">Unlimited (your own edges)</span>
          <p className="text-sm text-muted-foreground">
            Public traffic through your own edges and relays is never counted or limited by Paperboat. Previews and tunnels on them have no plan
            limits.
          </p>
        </>
      ) : (
        <>
          <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
            <span className="text-2xl font-medium tracking-tight tabular-nums">
              {formatBytes(fair.used)} <span className="text-base font-normal text-muted-foreground">of {formatBytes(fair.amount)} included</span>
            </span>
            <span className="text-sm text-muted-foreground" data-fair-use-state>
              {fair.throttled ? `Reduced to ${formatSpeed(fair.mbit)}${until ? ` until ${until}` : ""}` : "Full speed"}
            </span>
          </div>
          <div
            role="meter"
            aria-label="Included public traffic used"
            aria-valuemin={0}
            aria-valuemax={fair.amount}
            aria-valuenow={Math.min(fair.used, fair.amount)}
            className="flex h-1.5 overflow-hidden rounded-full bg-muted"
          >
            <div className={cn(fair.throttled ? "bg-muted-foreground" : "bg-primary")} style={{ width: `${fair.share * 100}%` }} />
          </div>
          <p className="text-sm text-muted-foreground">
            {formatBytes(fair.amount)} a month of public traffic included{until && !fair.throttled ? `, resets ${until}` : ""}. After that, speed is
            reduced to {formatSpeed(fair.mbit)} until your next billing date. Never cut off, never billed extra.
          </p>
          <p className="text-xs text-muted-foreground">
            Relays never count against this and are never cut off: {formatSpeed(fair.relay.mbit)} for the first {formatBytes(fair.relay.included)}{" "}
            a month, then {formatSpeed(fair.relay.slowMbit)}. Direct connections are not counted. Run your own edge or relay (free, open source): no
            limits.
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
