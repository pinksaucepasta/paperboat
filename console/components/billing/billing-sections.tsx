"use client"

import { MinusIcon, PlusIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { idempotencyKey, useApiQuery, type Workspace } from "@/lib/api"
import {
  billingLive,
  billingProductsQuery,
  billingQuery,
  buyTopup,
  topupDefaults,
  formatMoney,
  formatPrice,
  notLiveTitle,
  openPortal,
  seatPreviewQuery,
  setSeats,
  type BillingWorkspace,
  type ChangePreview,
} from "@/lib/billing"
import { formatBytes } from "@/lib/format"
import { Row, Section } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"

const gigabyte = 1e9

const dateFormat = new Intl.DateTimeFormat(undefined, { year: "numeric", month: "short", day: "numeric" })

/** Refetches the plan after any change so every row shows the server's state. */
function useRefreshBilling(workspace: Workspace) {
  const queryClient = useQueryClient()
  return () => queryClient.invalidateQueries({ queryKey: billingQuery(workspace).queryKey })
}

/** "Charged now $X · then $Y on Nov 18": the line every paid change shows before it is confirmed. */
function PreviewLine({ preview, pending }: { preview?: ChangePreview; pending: boolean }) {
  if (!billingLive.seats) return null
  if (pending || !preview) return <span className="text-muted-foreground">Calculating…</span>
  return (
    <span className="text-muted-foreground">
      {formatMoney(preview.amount_due_now, preview.currency)} now · then{" "}
      {formatMoney(preview.next_invoice_amount, preview.currency)} on{" "}
      {dateFormat.format(new Date(preview.next_invoice_at))}
    </span>
  )
}

/**
 * Seats are bought, members and pending invitations use them (NOTES.md "Teams"). Adding seats is
 * charged now, prorated to the billing date; removing them lowers the next invoice only.
 */
export function Seats({ workspace, billing }: { workspace: Workspace; billing: BillingWorkspace }) {
  const bought = billing.seats_bought ?? billing.paid_seats
  const used = billing.seats_used ?? billing.members
  const [quantity, setQuantity] = useState(bought)
  const refresh = useRefreshBilling(workspace)
  const changed = quantity !== bought
  const preview = useApiQuery({ ...seatPreviewQuery(workspace, quantity), enabled: billingLive.seats && changed })
  const save = useMutation({ mutationFn: () => setSeats(workspace, quantity), onSuccess: refresh })
  const live = billingLive.seats && billing.can_manage
  return (
    <Section title="Seats">
      <div className="flex min-h-12 flex-wrap items-center justify-between gap-x-6 gap-y-2 border-b border-divider py-2 text-sm">
        <span className="flex items-center gap-2">
          <Button
            variant="outline"
            size="icon-sm"
            aria-label="Remove a seat"
            disabled={!live || quantity <= Math.max(1, used)}
            title={live ? undefined : notLiveTitle}
            onClick={() => setQuantity(quantity - 1)}
          >
            <MinusIcon />
          </Button>
          <span className="min-w-16 text-center tabular-nums">
            {quantity} {quantity === 1 ? "seat" : "seats"}
          </span>
          <Button
            variant="outline"
            size="icon-sm"
            aria-label="Add a seat"
            disabled={!live}
            title={live ? undefined : notLiveTitle}
            onClick={() => setQuantity(quantity + 1)}
          >
            <PlusIcon />
          </Button>
        </span>
        {changed ? (
          <span className="flex items-center gap-3">
            <PreviewLine preview={preview.data} pending={preview.isPending} />
            <Button variant="ghost" onClick={() => setQuantity(bought)}>
              Reset
            </Button>
            <Button disabled={save.isPending} onClick={() => save.mutate()}>
              {quantity > bought ? "Buy seats" : "Remove seats"}
            </Button>
          </span>
        ) : (
          <span className="text-muted-foreground">
            {used} used · {Math.max(0, bought - used)} free
          </span>
        )}
      </div>
      {quantity < bought && changed && (
        <p className="py-2 text-sm text-muted-foreground">Removed seats stay paid until the billing date.</p>
      )}
      {save.isError && <p className="py-2 text-sm text-destructive">{save.error.message}</p>}
    </Section>
  )
}

/**
 * Monthly ↔ yearly and cancelling happen in Stripe's portal, which shows the prorated charge and
 * confirms the change; the webhook updates the plan here when the user returns.
 */
export function PlanActions({ workspace, billing }: { workspace: Workspace; billing: BillingWorkspace }) {
  const products = useApiQuery(billingProductsQuery(workspace))
  const portal = useMutation({
    mutationFn: () => openPortal(workspace, idempotencyKey()),
    onSuccess: (url) => window.location.assign(url),
  })
  const current = billing.product
  const other = current && products.data?.find((p) => p.interval !== current.interval && p.price_verified)
  if (!current || !billing.can_manage) return null
  return (
    <>
      {other && (
        <Row label="Billing period">
          <Button variant="outline" disabled={portal.isPending} onClick={() => portal.mutate()}>
            {other.interval === "year" ? "Switch to yearly" : "Switch to monthly"} · {formatPrice(other)}/
            {other.interval}
          </Button>
        </Row>
      )}
      <Row label={billing.cancel_at_period_end ? "Cancelled" : "Cancel"}>
        <Button variant="outline" disabled={portal.isPending} onClick={() => portal.mutate()}>
          {billing.cancel_at_period_end ? "Keep plan" : "Cancel plan"}
        </Button>
      </Row>
      {portal.isError && <p className="py-2 text-sm text-destructive">{portal.error.message}</p>}
    </>
  )
}

/**
 * Buy more hosted bandwidth on top of the plan: any multiple of 100 GB up to 10,000 GB at a time,
 * priced per 100 GB by the server's top-up product. It lasts until used.
 */
export function TopUps({ workspace, billing }: { workspace: Workspace; billing: BillingWorkspace }) {
  const products = useApiQuery({ ...billingProductsQuery(workspace), enabled: billingLive.topups })
  const product = products.data?.find((p) => (p.catalog_type as string) === "top_up" && p.price_verified)
  const step = product?.unit_bytes ?? topupDefaults.unitBytes
  const max = product?.max_quantity ?? topupDefaults.maxQuantity
  const [units, setUnits] = useState(1)
  const buy = useMutation({
    mutationFn: () => buyTopup(workspace, product!, units),
    onSuccess: (url) => window.location.assign(url),
  })
  const live = billingLive.topups && Boolean(product) && billing.can_manage
  const total = product?.unit_amount != null ? formatMoney(product.unit_amount * units, product.currency) : null
  return (
    <Section
      title="Add bandwidth"
      action={
        billing.topup_bytes_remaining ? (
          <span className="text-xs text-muted-foreground">{formatBytes(billing.topup_bytes_remaining)} extra left</span>
        ) : undefined
      }
    >
      <div className="flex min-h-14 flex-wrap items-center justify-between gap-x-6 gap-y-2 border-b border-divider py-2 text-sm">
        <span className="flex items-center gap-2">
          <Button
            variant="outline"
            size="icon-sm"
            aria-label="100 GB less"
            disabled={units <= 1}
            onClick={() => setUnits(units - 1)}
          >
            <MinusIcon />
          </Button>
          <label className="flex items-center gap-1.5">
            <Input
              type="number"
              aria-label="Gigabytes"
              className="w-24 text-right tabular-nums"
              min={step / gigabyte}
              max={(step * max) / gigabyte}
              step={step / gigabyte}
              value={(units * step) / gigabyte}
              onChange={(event) => {
                const gb = Number(event.target.value)
                if (Number.isFinite(gb)) setUnits(Math.min(max, Math.max(1, Math.round((gb * gigabyte) / step))))
              }}
            />
            <span className="text-muted-foreground">GB</span>
          </label>
          <Button
            variant="outline"
            size="icon-sm"
            aria-label="100 GB more"
            disabled={units >= max}
            onClick={() => setUnits(units + 1)}
          >
            <PlusIcon />
          </Button>
        </span>
        <span className="flex items-center gap-3">
          {total && <span className="tabular-nums">{total}</span>}
          <Button
            disabled={!live || buy.isPending}
            title={live ? undefined : notLiveTitle}
            onClick={() => buy.mutate()}
          >
            Buy
          </Button>
        </span>
      </div>
      <p className="py-2 text-xs text-muted-foreground">
        Used after the plan&apos;s allowance runs out. It doesn&apos;t expire or renew.
      </p>
      {buy.isError && <p className="py-2 text-sm text-destructive">{buy.error.message}</p>}
    </Section>
  )
}
