"use client"

import Link from "next/link"
import { WarningCircleIcon } from "@phosphor-icons/react"
import { allowanceWindow, billingQuery } from "@/lib/billing"
import { useApiQuery, useWorkspace } from "@/lib/api"
import { formatBytes } from "@/lib/format"
import { useTimeFormat } from "@/lib/use-time-format"

/** The workspace's hosted allowance when it is used up, else undefined. */
function useExhausted() {
  const workspace = useWorkspace()
  const billing = useApiQuery(billingQuery(workspace))
  const data = billing.data
  if (!workspace || !data || data.allowance_bytes === null || data.usage_bytes < data.allowance_bytes) return undefined
  return { workspace, data, allowance: data.allowance_bytes }
}

/**
 * What the cap means and what to do, in one place: the banner shows it on every page, and a
 * browser terminal the edge refused at the cap shows it instead of reconnecting.
 */
export function QuotaMessage({ fallbackName }: { fallbackName?: string }) {
  const exhausted = useExhausted()
  const time = useTimeFormat()
  if (!exhausted) {
    return (
      <p>
        {fallbackName ?? "This workspace"} has used its bandwidth. Relayed connections are paused. Direct
        machine-to-machine access still works.{" "}
        <Link href="/billing" className="font-medium underline underline-offset-4">
          Billing
        </Link>
      </p>
    )
  }
  const { workspace, data, allowance } = exhausted
  const window = allowanceWindow(data)
  const renews = window.end ? ` It renews ${time.date(window.end, "day")}.` : ""
  const plan = data.product?.name ?? "Free"
  const canBuy = workspace.kind === "personal" || workspace.role === "owner" || workspace.role === "admin"
  return (
    <p className="min-w-0 flex-1">
      <span className="font-medium">
        {workspace.name} has used its {formatBytes(allowance)} of bandwidth ({plan}).
      </span>{" "}
      <span className="text-muted-foreground">
        Previews, tunnels and relayed connections are paused.{renews} Direct machine-to-machine access still works.
        {!canBuy && " Ask an owner or admin to add bandwidth."}
      </span>
      {canBuy && (
        <span className="ml-3 inline-flex items-center gap-3">
          <Link href="/billing" className="font-medium underline-offset-4 hover:underline">
            Add bandwidth
          </Link>
          <Link href="/billing" className="font-medium underline-offset-4 hover:underline">
            Change plan
          </Link>
        </span>
      )}
    </p>
  )
}

/**
 * Shown on every page while this workspace's hosted bandwidth is used up: previews, tunnels and
 * relayed connections stop working until the allowance renews, so the console says so once, with
 * the plan's limit and what to do. Direct machine-to-machine traffic is free and keeps working.
 */
export function QuotaBanner() {
  if (!useExhausted()) return null
  return (
    <div
      role="status"
      data-quota-banner
      className="flex flex-wrap items-center gap-x-4 gap-y-2 border-b border-divider bg-surface px-4 py-3 text-sm md:px-6"
    >
      <WarningCircleIcon weight="fill" className="size-4 shrink-0 text-destructive" />
      <QuotaMessage />
    </div>
  )
}
