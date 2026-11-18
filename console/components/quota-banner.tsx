"use client"

import Link from "next/link"
import { GaugeIcon } from "@phosphor-icons/react"
import { allowanceWindow, billingQuery, formatSpeed } from "@/lib/billing"
import { useApiQuery, useWorkspace } from "@/lib/api"
import { useTimeFormat } from "@/lib/use-time-format"

/**
 * Shown on every page while this workspace's included public traffic is used up: speed through Paperboat's edges is
 * reduced until the period ends. Nothing is cut off; direct connections, relays and your own edges are not
 * affected. One sentence and what can be done about it.
 */
export function QuotaBanner() {
  const workspace = useWorkspace()
  const billing = useApiQuery(billingQuery(workspace))
  const time = useTimeFormat()
  const data = billing.data
  if (!workspace || !data?.throttled) return null
  const end = allowanceWindow(data).end
  const canBuy = workspace.kind === "personal" || workspace.role === "owner" || workspace.role === "admin"
  return (
    <div
      role="status"
      data-quota-banner
      className="flex flex-wrap items-center gap-x-4 gap-y-2 border-b border-divider bg-surface px-4 py-3 text-sm md:px-6"
    >
      <GaugeIcon weight="fill" className="size-4 shrink-0 text-muted-foreground" />
      <p className="min-w-0 flex-1">
        <span className="font-medium">
          {workspace.name} used its included public traffic. Speed is reduced to {formatSpeed(data.throttle_mbit)}
          {end ? ` until ${time.date(end, "day")}` : ""}.
        </span>{" "}
        <span className="text-muted-foreground">
          Never cut off, never billed extra.{!canBuy && " Ask an owner or admin to add bandwidth."}
        </span>
        {canBuy && (
          <span className="ml-3 inline-flex items-center gap-3">
            {!data.granted && (
              <Link href="/billing" className="font-medium underline-offset-4 hover:underline">
                Add bandwidth
              </Link>
            )}
            <Link href="/billing" className="font-medium underline-offset-4 hover:underline">
              {data.granted ? "Subscribe" : "Change plan"}
            </Link>
          </span>
        )}
      </p>
    </div>
  )
}
