"use client"

import { MinusIcon, PlusIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { useApiQuery, type Workspace } from "@/lib/api"
import {
  billingLive,
  billingProductsQuery,
  billingQuery,
  buyTopup,
  topupDefaults,
  changePlan,
  formatMoney,
  formatPrice,
  notLiveTitle,
  planPreviewQuery,
  seatPreviewQuery,
  setCancelled,
  setSeats,
  type BillingProduct,
  type BillingWorkspace,
  type ChangePreview,
} from "@/lib/billing"
import { formatBytes } from "@/lib/format"
import { Row, Section } from "@/components/section"
import { ConfirmAction } from "@/components/list-toolbar"
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
  if (!billingLive.seats && !billingLive.planChange) return null
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

/** Monthly ↔ yearly on the same plan, and cancel or resume at the end of the period. */
export function PlanActions({ workspace, billing }: { workspace: Workspace; billing: BillingWorkspace }) {
  const products = useApiQuery(billingProductsQuery(workspace))
  const current = billing.product
  const other = current && products.data?.find((p) => p.interval !== current.interval && p.price_verified)
  if (!current || !billing.can_manage) return null
  return (
    <>
      {other && <SwitchInterval workspace={workspace} product={other} />}
      <CancelRow workspace={workspace} billing={billing} />
    </>
  )
}

function SwitchInterval({ workspace, product }: { workspace: Workspace; product: BillingProduct }) {
  const [open, setOpen] = useState(false)
  const refresh = useRefreshBilling(workspace)
  const preview = useApiQuery({ ...planPreviewQuery(workspace, product), enabled: billingLive.planChange && open })
  const label = product.interval === "year" ? "Switch to yearly" : "Switch to monthly"
  return (
    <Row label="Billing period">
      <Button
        variant="outline"
        disabled={!billingLive.planChange}
        title={billingLive.planChange ? undefined : notLiveTitle}
        onClick={() => setOpen(true)}
      >
        {label} · {formatPrice(product)}/{product.interval}
      </Button>
      <ConfirmAction
        open={open}
        onOpenChange={setOpen}
        title={`${label}?`}
        description={<PreviewLine preview={preview.data} pending={preview.isPending} />}
        action="Switch"
        pending="Switching…"
        mutation={{ mutationFn: () => changePlan(workspace, product), onSuccess: refresh }}
      />
    </Row>
  )
}

function CancelRow({ workspace, billing }: { workspace: Workspace; billing: BillingWorkspace }) {
  const [open, setOpen] = useState(false)
  const refresh = useRefreshBilling(workspace)
  const end = billing.period_end ? dateFormat.format(new Date(billing.period_end)) : "the end of the period"
  const resume = useMutation({ mutationFn: () => setCancelled(workspace, false), onSuccess: refresh })
  const title = billingLive.cancel ? undefined : notLiveTitle
  return (
    <Row label={billing.cancel_at_period_end ? "Cancelled" : "Cancel"}>
      {billing.cancel_at_period_end ? (
        <Button
          variant="outline"
          disabled={!billingLive.cancel || resume.isPending}
          title={title}
          onClick={() => resume.mutate()}
        >
          Keep plan
        </Button>
      ) : (
        <Button variant="outline" disabled={!billingLive.cancel} title={title} onClick={() => setOpen(true)}>
          Cancel plan
        </Button>
      )}
      <ConfirmAction
        open={open}
        onOpenChange={setOpen}
        title="Cancel plan?"
        description={`Paid features and the larger allowance stay until ${end}, then the workspace moves to Free. No refund for the current period.`}
        action="Cancel plan"
        pending="Cancelling…"
        mutation={{ mutationFn: () => setCancelled(workspace, true), onSuccess: refresh }}
      />
    </Row>
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
