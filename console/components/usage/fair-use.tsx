"use client"

import Link from "next/link"
import { billingQuery, fairUse } from "@/lib/billing"
import { useApiQuery, useWorkspace } from "@/lib/api"
import { formatBytes } from "@/lib/format"
import { useTimeFormat } from "@/lib/use-time-format"
import { cn } from "@/lib/utils"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"

/**
 * Edge traffic is "Unlimited*": the fair-use amount, what is used of it, and (past it) the slower speed and when
 * full speed returns. Never an error: nothing is stopped. Adding bandwidth or changing plan lifts it at once.
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
  const nearly = fair.throttled || fair.share >= 0.8
  return (
    <section
      data-fair-use
      data-throttled={fair.throttled}
      aria-label="Fair-use traffic"
      className="flex flex-col gap-3 rounded-lg border border-divider bg-surface p-4"
    >
      <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-1">
        <span className="text-2xl font-medium tracking-tight">Unlimited*</span>
        <span className="text-sm text-muted-foreground" data-fair-use-state>
          {fair.throttled
            ? `Slowed to ${fair.mbit} Mbit/s${until ? ` until ${until}` : ""}`
            : "Full speed"}
        </span>
      </div>
      <div
        role="meter"
        aria-label="Fair-use traffic used"
        aria-valuemin={0}
        aria-valuemax={fair.amount}
        aria-valuenow={Math.min(fair.used, fair.amount)}
        className="flex h-1.5 overflow-hidden rounded-full bg-muted"
      >
        <div className={cn(fair.throttled ? "bg-muted-foreground" : "bg-primary")} style={{ width: `${fair.share * 100}%` }} />
      </div>
      <p className="text-sm tabular-nums">
        {formatBytes(fair.used)} <span className="text-muted-foreground">of {formatBytes(fair.amount)} fair use</span>
        {until && !fair.throttled && <span className="text-muted-foreground"> · resets {until}</span>}
      </p>
      <p className="text-xs text-muted-foreground">
        *Fair use: after {formatBytes(fair.amount)} a month on Paperboat&apos;s edges, speed is reduced to {fair.mbit} Mbit/s until your next
        billing date. Direct connections, relays, and your own (self-hosted) edges and relays are never limited.
      </p>
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
