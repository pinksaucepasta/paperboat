"use client"

import { CircleNotchIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useEffect, useRef, useState } from "react"
import { ApiError, idempotencyKey, useApiQuery, useWorkspace, type Workspace } from "@/lib/api"
import {
  billingProductsQuery,
  allowanceWindow,
  formatAllowance,
  planEnded,
  billingQuery,
  formatPrice,
  openPortal,
  startCheckout,
  type BillingInterval,
  type BillingProduct,
  type BillingWorkspace,
  formatLimit,
} from "@/lib/billing"
import { PlanActions, Seats, TopUps } from "@/components/billing/billing-sections"
import { Row, Section } from "@/components/section"
import { useTrack } from "@/lib/analytics"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { CheckoutConsent } from "@/components/billing/checkout-consent"
import { useTimeFormat } from "@/lib/use-time-format"
import { ErrorMessage } from "@/components/error-reference"

const intervals: Record<BillingInterval, string> = { month: "Monthly", year: "Yearly" }
/** How long the page waits for Stripe's webhook to activate a plan after Checkout returns. */
const confirmFor = 60_000

export function BillingPage() {
  const time = useTimeFormat()
  const workspace = useWorkspace()
  const billing = useApiQuery(billingQuery(workspace))
  const confirming = useConfirmingCheckout(workspace, billing.data)

  // The clock is 0 until the page has hydrated. Whether a plan has ended depends on it (an ended grant), and so does the return
  // from Checkout, so until then the page shows no plan at all rather than one it may have to take back.
  if (!workspace || billing.isPending || !time.now) return <BillingSkeleton />
  if (billing.isError)
    return (
      <p className="text-sm text-muted-foreground">
        <ErrorMessage error={billing.error} />
      </p>
    )
  const subscribed = Boolean(billing.data.product_code) && !planEnded(billing.data, time.now)
  return (
    <div className="flex flex-col gap-10">
      <Plan workspace={workspace} billing={billing.data} confirming={confirming} />
      {(!subscribed || billing.data.granted) && !confirming && <Plans workspace={workspace} billing={billing.data} />}
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
      ? // eslint-disable-next-line no-restricted-syntax -- browser only (window check); nothing renders from it before hydration
        Date.now()
      : null,
  )
  useEffect(() => {
    const url = new URL(window.location.href)
    if (!url.searchParams.has("checkout")) return
    url.searchParams.delete("checkout")
    window.history.replaceState(null, "", url)
  }, [])
  // A granted workspace has its plan already: the payment is confirmed once the grant has gone.
  const active = since !== null && (!billing?.product_code || Boolean(billing.granted))
  const track = useTrack()
  const product = billing?.product_code
  const waited = useRef(false)
  useEffect(() => {
    // Counted when the server shows the plan after the page waited for it; a top-up on an existing plan is not.
    if (active && billing) waited.current = true
    else if (waited.current && product) {
      waited.current = false
      track("checkout_completed", { workspace_kind: workspace?.kind ?? "personal", product })
    }
  }, [active, billing, product, workspace?.kind, track])
  useEffect(() => {
    if (!active || !workspace) return
    const timer = setInterval(() => {
      // eslint-disable-next-line no-restricted-syntax -- in an interval, not during render
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
  const time = useTimeFormat()
  // Stripe keeps retrying a failed renewal; paid features last until `paid_benefits_until`.
  const failed = ["past_due", "unpaid", "suspended"].includes(billing.state)
  const trialing = billing.state === "trialing"
  const benefits = billing.paid_benefits_until
  // Yearly plans renew once a year but their bandwidth resets monthly.
  const resets = allowanceWindow(billing).end
  const end = billing.period_end && time.date(billing.period_end, "date")
  // A support grant has no Stripe subscription, so there is nothing to manage in Stripe until the customer subscribes.
  const grantEnd = billing.granted && billing.granted_until && time.date(billing.granted_until, "date")
  const ended = planEnded(billing, time.now)
  const manage = billing.product_code && billing.can_manage && !billing.granted && !ended && (
    <Button variant="outline" size="lg" disabled={portal.isPending} onClick={() => portal.mutate()}>
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
        ) : ended ? (
          "Free"
        ) : billing.product && billing.granted ? (
          `${planName(billing.product)} · Granted`
        ) : billing.product ? (
          `${planName(billing.product)} · ${formatPrice(billing.product)}/${billing.product.interval}`
        ) : (
          // A plan granted outside the catalog (test data) has a code but no price or name.
          (billing.product_code ? codeName(billing.product_code) : "Free")
        )}
      </Row>
      {failed ? (
        <div className="flex items-center justify-between gap-6 border-b border-divider py-3 text-sm">
          <span className="text-destructive">
            {benefits && time.now && Date.parse(benefits) > time.now
              ? `Payment failed. Paid features stay until ${time.date(benefits, "date")}.`
              : "Payment failed. Paid features are paused until payment succeeds."}
          </span>
          {billing.can_manage && (
            <Button disabled={portal.isPending} onClick={() => portal.mutate()}>
              Update payment method
            </Button>
          )}
        </div>
      ) : trialing ? (
        // A Team trial (14 days, card at start): billing starts at period_end unless it is cancelled.
        <div className="flex items-center justify-between gap-6 border-b border-divider py-3 text-sm">
          <span>
            {billing.cancel_at_period_end
              ? `Free trial until ${end}. It is cancelled, so nothing will be charged.`
              : `Free trial until ${end}. Billing starts then; cancel before and you won’t be charged.`}
          </span>
          {billing.can_manage && !billing.cancel_at_period_end && (
            <Button variant="outline" disabled={portal.isPending} onClick={() => portal.mutate()}>
              Cancel trial
            </Button>
          )}
        </div>
      ) : (
        stateLabel(billing.state) && <Row label="Status">{stateLabel(billing.state)}</Row>
      )}
      {ended && end && <Row label="Plan ended">{end}</Row>}
      {grantEnd && <Row label="Granted until">{grantEnd}</Row>}
      {end && !trialing && !billing.granted && !ended && (
        <Row label={billing.cancel_at_period_end ? "Ends" : billing.product_code ? "Renews" : "Bandwidth resets"}>
          {end}
        </Row>
      )}
      {resets && resets !== billing.period_end && billing.product_code && (
        <Row label="Bandwidth resets">{time.date(resets, "date")}</Row>
      )}
      <Row label="Traffic included">{formatAllowance(billing)}</Row>
      {billing.limits && (
        <>
          <Row label="Machines">{formatLimit(billing.limits.machines)}</Row>
          <Row label="Previews & tunnels">{formatLimit(billing.limits.publications)}</Row>
          <Row label="Browser terminals">{formatLimit(billing.limits.browser_terminals)}</Row>
          <Row label="Public TCP tunnels">
            {formatLimit(billing.limits.public_tcp)}
          </Row>
        </>
      )}
      {!ended && <PlanActions workspace={workspace} billing={billing} />}
      {portal.isError && (
        <p className="py-2 text-sm text-destructive">
          <ErrorMessage error={portal.error} />
        </p>
      )}
    </Section>
  )
}

/** The plans this workspace can buy, monthly or yearly, each straight to Checkout. */
function Plans({ workspace, billing }: { workspace: Workspace; billing: BillingWorkspace }) {
  const subscribe = billing.granted
  const products = useApiQuery(billingProductsQuery(workspace))
  const [interval, setPeriod] = useState<BillingInterval>("month")
  const [terms, setTerms] = useState<string | null>(null)
  if (products.isPending) return <Skeleton className="h-24 w-full" />
  if (products.isError)
    return (
      <p className="text-sm text-muted-foreground">
        <ErrorMessage error={products.error} />
      </p>
    )
  const shown = products.data.filter((product) => product.interval === interval && product.price_verified)
  if (!products.data.length) return null
  return (
    <Section
      title={subscribe ? "Subscribe" : "Upgrade"}
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
        <PlanOffer key={product.code} workspace={workspace} product={product} canManage={billing.can_manage} terms={terms} verb={subscribe ? "Subscribe" : "Upgrade"} />
      ))}
      <CheckoutConsent terms={terms} onChange={setTerms} />
    </Section>
  )
}

function PlanOffer({
  workspace,
  product,
  canManage,
  terms,
  verb,
}: {
  workspace: Workspace
  product: BillingProduct
  canManage: boolean
  terms: string | null
  verb: string
}) {
  const queryClient = useQueryClient()
  // One key per purchase attempt. A 503 "outcome unknown" keeps it so a retry cannot buy twice;
  // any definite answer starts a fresh attempt.
  const [key, setKey] = useState(idempotencyKey)
  const checkout = useMutation({
    mutationFn: () => startCheckout(workspace, product, key, terms!),
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
          disabled={!canManage || terms === null || checkout.isPending}
          title={canManage ? (terms !== null ? undefined : "Tick the box below to continue.") : "Only the workspace owner can change billing."}
          onClick={() => checkout.mutate()}
        >
          {checkout.isPending && <CircleNotchIcon className="animate-spin" />}
          {checkout.isError && (checkout.error as ApiError).code === "provider_outcome_unknown"
            ? "Try again"
            : verb}
        </Button>
      </div>
      {checkout.isError && (
        <p className="text-destructive">
          <ErrorMessage error={checkout.error} />
        </p>
      )}
    </div>
  )
}

/** "Paperboat Pro (month)" → "Pro": the interval has its own toggle and the brand is implied. */
function planName(product: BillingProduct) {
  return product.name.replace(/^Paperboat\s+/, "").replace(/\s*\((month|year)\)$/, "")
}

/** "team-pro" → "Team Pro": the name of a plan the catalog has no display name for. */
function codeName(code: string) {
  return code
    .split(/[-_]/)
    .map((word) => word.charAt(0).toUpperCase() + word.slice(1))
    .join(" ")
}

/** The subscription's state in plain words; a state this console doesn't know shows no Status row. */
function stateLabel(state: string): string | undefined {
  const labels: Record<string, string> = {
    canceled: "Canceled",
    incomplete: "Payment incomplete",
    incomplete_expired: "Payment didn’t go through",
    past_due: "Payment overdue",
    unpaid: "Payment overdue",
    pending: "Payment processing",
    trialing: "Trial",
  }
  return labels[state]
}

/**
 * The plan page before billing is known (the prerendered shell): its headings, row labels and
 * buttons as a subscribed plan shows them, with placeholders where the plan's own values go.
 */
function BillingSkeleton() {
  const value = (width: string) => <Skeleton className={`my-[4px] ml-auto h-3 ${width}`} />
  const button = (width: string) => <Skeleton className={`ml-auto h-7 ${width}`} />
  return (
    <div className="flex flex-col gap-10">
      <Section title="Plan" action={<Skeleton className="h-8 w-28" />}>
        {["Plan", "Renews", "Traffic included", "Machines", "Previews & tunnels", "Browser terminals", "Public TCP tunnels"].map(
          (label) => (
            <Row key={label} label={label}>
              {value("w-24")}
            </Row>
          ),
        )}
        <Row label="Billing period">{button("w-40")}</Row>
        <Row label="Cancel">{button("w-24")}</Row>
      </Section>
      <Section title="Add bandwidth">
        <div className="flex min-h-14 items-center justify-between gap-x-6 border-b border-divider py-2 text-sm">
          <span className="flex items-center gap-2">
            <Skeleton className="size-6" />
            <Skeleton className="h-7 w-24" />
            <span className="text-muted-foreground">GB</span>
            <Skeleton className="size-6" />
          </span>
          <span className="flex items-center gap-3">
            <Skeleton className="h-3 w-8" />
            <Skeleton className="h-7 w-12" />
          </span>
        </div>
        {/* A div: a skeleton is a div, and one inside a <p> is invalid HTML (a hydration error now that this renders on the server). */}
        <div className="py-2 text-xs text-muted-foreground">
          Adds to this billing period. Unused bandwidth ends when your plan renews on{" "}
          <Skeleton className="inline-block h-3 w-20 align-middle" />.
        </div>
      </Section>
    </div>
  )
}
