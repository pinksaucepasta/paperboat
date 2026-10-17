"use client"

import { CircleNotchIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useEffect, useState } from "react"
import { ApiError, idempotencyKey, useApiQuery, useWorkspace, type Workspace } from "@/lib/api"
import {
  billingProductsQuery,
  billingQuery,
  formatPrice,
  openPortal,
  startCheckout,
  type BillingInterval,
  type BillingProduct,
  type BillingWorkspace,
} from "@/lib/billing"
import { formatBytes } from "@/lib/format"
import { PlanActions, Seats, TopUps } from "@/components/billing/billing-sections"
import { Row, Section } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"

const dateFormat = new Intl.DateTimeFormat(undefined, { year: "numeric", month: "short", day: "numeric" })
const intervals: Record<BillingInterval, string> = { month: "Monthly", year: "Yearly" }
/** How long the page waits for Stripe's webhook to activate a plan after Checkout returns. */
const confirmFor = 60_000

export function BillingPage() {
  const workspace = useWorkspace()
  const billing = useApiQuery(billingQuery(workspace))
  const confirming = useConfirmingCheckout(workspace, billing.data)

  if (!workspace || billing.isPending) return <Skeleton className="h-40 w-full" />
  if (billing.isError) return <p className="text-sm text-muted-foreground">{billing.error.message}</p>
  const subscribed = Boolean(billing.data.product_code)
  return (
    <div className="flex flex-col gap-10">
      <Plan workspace={workspace} billing={billing.data} confirming={confirming} />
      {!subscribed && !confirming && <Plans workspace={workspace} billing={billing.data} />}
      {subscribed && workspace.kind === "team" && <Seats workspace={workspace} billing={billing.data} />}
      <TopUps workspace={workspace} billing={billing.data} />
    </div>
  )
}

/**
 * After Checkout returns to `?checkout=done`, the plan activates when Stripe's webhook lands, a
 * few seconds later. Until then the page polls and says so; it gives up after a minute.
 */
function useConfirmingCheckout(workspace: Workspace | undefined, billing: BillingWorkspace | undefined) {
  const queryClient = useQueryClient()
  // Read once on the first client render; nothing shows before hydration (the plan is loading).
  const [since, setSince] = useState(() =>
    typeof window !== "undefined" && new URL(window.location.href).searchParams.get("checkout") === "done"
      ? Date.now()
      : null,
  )
  useEffect(() => {
    const url = new URL(window.location.href)
    if (!url.searchParams.has("checkout")) return
    url.searchParams.delete("checkout")
    window.history.replaceState(null, "", url)
  }, [])
  const active = since !== null && !billing?.product_code
  useEffect(() => {
    if (!active || !workspace) return
    const timer = setInterval(() => {
      if (Date.now() - since! > confirmFor) return setSince(null)
      void queryClient.invalidateQueries({ queryKey: billingQuery(workspace).queryKey })
    }, 2000)
    return () => clearInterval(timer)
  }, [active, since, workspace, queryClient])
  return active
}

function Plan({
  workspace,
  billing,
  confirming,
}: {
  workspace: Workspace
  billing: BillingWorkspace
  confirming: boolean
}) {
  const portal = useMutation({
    mutationFn: () => openPortal(workspace, idempotencyKey()),
    onSuccess: (url) => window.location.assign(url),
  })
  // Stripe keeps retrying a failed renewal; paid features last until `paid_benefits_until`.
  const failed = ["past_due", "unpaid", "suspended"].includes(billing.state)
  const benefits = billing.paid_benefits_until
  const end = billing.period_end && dateFormat.format(new Date(billing.period_end))
  const manage = billing.product_code && billing.can_manage && (
    <Button variant="outline" disabled={portal.isPending} onClick={() => portal.mutate()}>
      {portal.isPending && <CircleNotchIcon className="animate-spin" />}
      Manage billing
    </Button>
  )
  return (
    <Section title="Plan" action={manage}>
      <Row label="Plan">
        {confirming ? (
          <span className="flex items-center gap-1.5 text-muted-foreground">
            <CircleNotchIcon className="size-3.5 animate-spin" /> Confirming payment…
          </span>
        ) : billing.product ? (
          `${planName(billing.product)} · ${formatPrice(billing.product)}/${billing.product.interval}`
        ) : (
          // A plan granted outside the catalog (test data) has a code but no price.
          (billing.product_code ?? "Free")
        )}
      </Row>
      {failed ? (
        <div className="flex items-center justify-between gap-6 border-b border-divider py-3 text-sm">
          <span className="text-destructive">
            {benefits && new Date(benefits) > new Date()
              ? `Payment failed. Paid features stay until ${dateFormat.format(new Date(benefits))}.`
              : "Payment failed. Paid features are paused until payment succeeds."}
          </span>
          {billing.can_manage && (
            <Button disabled={portal.isPending} onClick={() => portal.mutate()}>
              Update payment method
            </Button>
          )}
        </div>
      ) : (
        billing.state !== "free" && billing.state !== "active" && <Row label="Status">{stateLabel(billing.state)}</Row>
      )}
      {end && (
        <Row label={billing.cancel_at_period_end ? "Ends" : billing.product_code ? "Renews" : "Resets"}>{end}</Row>
      )}
      <Row label="Hosted allowance">
        {billing.allowance_bytes === null
          ? `${formatBytes(billing.usage_bytes)} used`
          : `${formatBytes(billing.usage_bytes)} of ${formatBytes(billing.allowance_bytes)} used`}
      </Row>
      <PlanActions workspace={workspace} billing={billing} />
      {portal.isError && <p className="py-2 text-sm text-destructive">{portal.error.message}</p>}
    </Section>
  )
}

/** The plans this workspace can buy, monthly or yearly, each straight to Checkout. */
function Plans({ workspace, billing }: { workspace: Workspace; billing: BillingWorkspace }) {
  const products = useApiQuery(billingProductsQuery(workspace))
  const [interval, setPeriod] = useState<BillingInterval>("month")
  if (products.isPending) return <Skeleton className="h-24 w-full" />
  if (products.isError) return <p className="text-sm text-muted-foreground">{products.error.message}</p>
  const shown = products.data.filter((product) => product.interval === interval && product.price_verified)
  if (!products.data.length) return null
  return (
    <Section
      title="Upgrade"
      action={
        <ToggleGroup
          value={[interval]}
          onValueChange={(next) => next[0] && setPeriod(next[0] as BillingInterval)}
          variant="outline"
          spacing={0}
        >
          {Object.entries(intervals).map(([value, label]) => (
            <ToggleGroupItem key={value} value={value}>
              {label}
            </ToggleGroupItem>
          ))}
        </ToggleGroup>
      }
    >
      {shown.map((product) => (
        <PlanOffer key={product.code} workspace={workspace} product={product} canManage={billing.can_manage} />
      ))}
    </Section>
  )
}

function PlanOffer({
  workspace,
  product,
  canManage,
}: {
  workspace: Workspace
  product: BillingProduct
  canManage: boolean
}) {
  const queryClient = useQueryClient()
  // One key per purchase attempt. A 503 "outcome unknown" keeps it so a retry cannot buy twice;
  // any definite answer starts a fresh attempt.
  const [key, setKey] = useState(idempotencyKey)
  const checkout = useMutation({
    mutationFn: () => startCheckout(workspace, product, key),
    onSuccess: (url) => window.location.assign(url),
    onError: (error) => {
      if (error instanceof ApiError && error.code === "provider_outcome_unknown") return
      setKey(idempotencyKey())
      if (error instanceof ApiError && error.code === "product_changed")
        void queryClient.invalidateQueries({ queryKey: billingProductsQuery(workspace).queryKey })
      if (error instanceof ApiError && error.code === "subscription_exists")
        void queryClient.invalidateQueries({ queryKey: billingQuery(workspace).queryKey })
    },
  })
  const seat = workspace.kind === "team" ? " per seat" : ""
  return (
    <div className="flex flex-col gap-1 border-b border-divider py-3 text-sm">
      <div className="flex items-center justify-between gap-6">
        <span className="flex flex-col gap-1">
          <span className="font-medium">{planName(product)}</span>
          <span className="text-muted-foreground">
            {formatPrice(product)}/{product.interval}
            {seat}
          </span>
        </span>
        <Button
          disabled={!canManage || checkout.isPending}
          title={canManage ? undefined : "Only the workspace owner can change billing."}
          onClick={() => checkout.mutate()}
        >
          {checkout.isPending && <CircleNotchIcon className="animate-spin" />}
          {checkout.isError && (checkout.error as ApiError).code === "provider_outcome_unknown"
            ? "Try again"
            : "Upgrade"}
        </Button>
      </div>
      {checkout.isError && <p className="text-destructive">{checkout.error.message}</p>}
    </div>
  )
}

/** "Paperboat Pro (month)" → "Pro": the interval has its own toggle and the brand is implied. */
function planName(product: BillingProduct) {
  return product.name.replace(/^Paperboat\s+/, "").replace(/\s*\((month|year)\)$/, "")
}

function stateLabel(state: string) {
  const labels: Record<string, string> = {
    canceled: "Canceled",
    incomplete: "Payment incomplete",
    pending: "Payment processing",
  }
  return labels[state] ?? state.replace(/_/g, " ")
}
