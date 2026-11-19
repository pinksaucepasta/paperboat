"use client"

import { queryOptions } from "@tanstack/react-query"
import { formatBytes } from "@/lib/format"
import { queryKeys } from "@/lib/query-keys"
import { api, idempotencyKey, type Data, type Schemas, type Workspace } from "@/lib/api"
import type { components } from "@/lib/api-schema"

/** `limit` is null where the plan is unlimited (public_tcp: only on your own tunnel edge). */
type Limit = { used: number; limit: number | null; granted?: boolean; self_hosted?: boolean }
export type BillingWorkspace = components["schemas"]["BillingWorkspace"]

/** "6 of 1,000", "6 · Unlimited", or "6 · Unlimited (your own edges)" when the workspace runs its own edges. */
export const formatLimit = ({ used, limit, granted, self_hosted }: Limit) =>
  limit === null
    ? `${used.toLocaleString("en-US")} · Unlimited${self_hosted ? " (your own edges)" : ""}`
    : `${used.toLocaleString("en-US")} of ${limit.toLocaleString("en-US")}${granted ? " · Granted" : ""}`

/** The fair-use sentence: "5.0 GB of 100 GB used", with what support granted when some of it is. */
export const formatAllowance = (billing: BillingWorkspace) =>
  billing.allowance_bytes === null
    ? `${formatBytes(billing.usage_bytes)} used`
    : `${formatBytes(billing.usage_bytes)} of ${formatBytes(billing.allowance_bytes)} used${
        billing.allowance_granted_bytes ? ` · ${formatBytes(billing.allowance_granted_bytes)} granted` : ""
      }`

/** "512 kbit/s", "1 Mbit/s". */
export const formatSpeed = (mbit: number) => (mbit < 1 ? `${Math.round(mbit * 1000)} kbit/s` : `${mbit} Mbit/s`)

/**
 * Traffic through Paperboat's edges and relays (previews, tunnels, relayed connections; files uploaded through a browser terminal; its typing and output are not counted) is ONE pool, included up
 * to an amount a month; past it speed (edges and relays alike) is reduced to `mbit` Mbit/s until the period ends, never cut off or billed extra. The amount is the plan's plus
 * what support granted and what was topped up. `ownEdges`: the workspace runs its own edges, so nothing is counted or limited.
 * Undefined when the workspace has no included amount.
 */
export function fairUse(billing: BillingWorkspace) {
  const ownEdges = Boolean(billing.limits?.publications.self_hosted)
  const relay = { mbit: billing.relay_mbit }
  // A paid team's amounts are per seat, pooled in the team: "100 GB × 4 seats".
  const seats = billing.allowance_seats ? { count: billing.allowance_seats, perSeat: billing.allowance_per_seat_bytes ?? 0 } : undefined
  if (ownEdges) return { ownEdges, relay, seats, amount: 0, used: billing.usage_bytes, share: 0, throttled: false, mbit: 0, ends: undefined }
  if (billing.allowance_bytes === null) return undefined
  const amount = billing.allowance_bytes + billing.topup_bytes_purchased
  const used = billing.usage_bytes
  return {
    ownEdges,
    relay,
    seats,
    amount,
    used,
    share: amount > 0 ? Math.min(1, used / amount) : 1,
    throttled: billing.throttled,
    mbit: billing.throttle_mbit,
    ends: allowanceWindow(billing).end,
  }
}
export type BillingProduct = components["schemas"]["WorkspaceBillingProduct"]
export type BillingInterval = "month" | "year"

/**
 * Billing features whose endpoints the console may call. An unknown /v1 path answers 401, which
 * signs the user out (NOTES.md), so the UI for a feature is built but disabled until its endpoint
 * is deployed. Flip one when Lane B logs the deploy.
 */
export const billingLive = {
  seats: true,
  topups: true,
  paidTeamCreate: true,
} as const
export const notLiveTitle = "Not available yet"

/** What a change costs now and from the next invoice; Stripe prorates to the fixed billing date. */
export type ChangePreview = Schemas["SeatPreview"]

const minute = 60_000

/** The workspace's billing routes: personal, or one team by ID. */
function billingPath(workspace: Workspace) {
  return workspace.kind === "team"
    ? `billing/workspaces/teams/${encodeURIComponent(workspace.id)}`
    : "billing/workspaces/personal"
}

export const billingKey = queryKeys.billing

/** The workspace's plan and its hosted relay + edge allowance for the current period. */
export const billingQuery = (workspace: Workspace | undefined) =>
  queryOptions({
    queryKey: billingKey(workspace?.id ?? ""),
    queryFn: ({ signal }) => api<BillingWorkspace>(billingPath(workspace!), { signal }),
    enabled: Boolean(workspace),
    staleTime: minute,
  })

/**
 * The bandwidth window: the server's when it sends one; else the plan period for monthly plans and
 * Free. A yearly period is not the bandwidth window, so then only its start is known (as "since").
 */
/**
 * A plan whose period has ended while it still reads as active: a grant that ran out, or a cancelled plan whose
 * webhook has not landed yet. It no longer gives paid features, so the console shows it as ended, not as the plan.
 */
export function planEnded(billing: BillingWorkspace, now: number | undefined): boolean {
  return Boolean(
    billing.product_code &&
      billing.state === "active" &&
      billing.cancel_at_period_end &&
      billing.period_end &&
      now &&
      Date.parse(billing.period_end) < now,
  )
}

export function allowanceWindow(billing: BillingWorkspace): { start?: string; end?: string } {
  if (billing.allowance_period_end) return { start: billing.allowance_period_start, end: billing.allowance_period_end }
  if (billing.product?.interval === "year") return {}
  return { start: billing.period_start, end: billing.period_end }
}

/**
 * Where this period's usage starts: the allowance window, or the calendar month when there is none.
 * The allowance and bandwidth cards both count from here, so their figures agree.
 */
export function usageWindowStart(billing: BillingWorkspace, now = new Date()): string {
  return allowanceWindow(billing).start ?? new Date(now.getFullYear(), now.getMonth(), 1).toISOString()
}

/** Plans this workspace can buy, with verified prices. Personal sees Pro, a team sees Team. */
export const billingProductsQuery = (workspace: Workspace | undefined) =>
  queryOptions({
    queryKey: queryKeys.billingProducts(workspace?.id ?? ""),
    queryFn: async ({ signal }) =>
      (await api<Data<"/v1/billing/workspaces/personal/products">>(`${billingPath(workspace!)}/products`, { signal })).items,
    enabled: Boolean(workspace),
    staleTime: 5 * minute,
  })

/**
 * Starts Stripe Checkout and returns its URL. `key` is the operation's idempotency key: a retry
 * after an unknown provider outcome must send the same key so it cannot buy twice.
 */
export async function startCheckout(workspace: Workspace, product: BillingProduct, key: string, termsVersion: string) {
  const back = `${window.location.origin}/billing`
  const { url } = await api<{ url: string }>(`${billingPath(workspace)}/checkout`, {
    method: "POST",
    headers: { "content-type": "application/json", "idempotency-key": key },
    body: JSON.stringify({
      product_code: product.code,
      product_version: product.version,
      success_url: `${back}?checkout=done`,
      cancel_url: back,
      terms_version: termsVersion,
    }),
  })
  return url
}

/** Opens Stripe's billing portal: payment method, invoices, plan changes and cancel. */
export async function openPortal(workspace: Workspace, key: string) {
  const { url } = await api<{ url: string }>(`${billingPath(workspace)}/portal`, {
    method: "POST",
    headers: { "content-type": "application/json", "idempotency-key": key },
    body: JSON.stringify({ return_url: `${window.location.origin}/billing` }),
  })
  return url
}

/** "$8", "$80", "$15.50": whole amounts without cents. */
export function formatPrice(product: BillingProduct) {
  if (product.unit_amount === null) return "Price unavailable"
  return formatMoney(product.unit_amount, product.currency)
}

const post = (path: string, body: object, key = idempotencyKey()) =>
  api<unknown>(path, {
    method: "POST",
    headers: { "content-type": "application/json", "idempotency-key": key },
    body: JSON.stringify(body),
  })

export const seatPreviewQuery = (workspace: Workspace, quantity: number) =>
  queryOptions({
    queryKey: ["billing-seats-preview", workspace.id, quantity],
    queryFn: ({ signal }) =>
      api<ChangePreview>(`${billingPath(workspace)}/seats/preview?quantity=${quantity}`, { signal }),
    enabled: billingLive.seats,
  })

/** Buys or removes seats. Added seats are invoiced now; removed ones stay paid until the billing date. */
export const setSeats = (workspace: Workspace, quantity: number) =>
  post(`${billingPath(workspace)}/seats`, { quantity })

/** The SSO and SCIM add-ons of a team ("" is none); their products are `addon-<kind>-<interval>`. */
export type Addon = "" | "sso" | "scim" | "sso_scim"
export const addonNames: Record<Addon, string> = { "": "None", sso: "SSO", scim: "SCIM", sso_scim: "SSO and SCIM" }

/** The add-on product a team on this interval would buy, with its price. */
export const addonProduct = (products: BillingProduct[] | undefined, addon: Addon, interval: string) =>
  products?.find((p) => p.code === `addon-${addon}-${interval}` && p.price_verified)

/**
 * Sets the team's add-on ("" removes it). Adding or switching charges the prorated difference now, so the server needs the
 * buyer's acceptance of the refund policy (the checkout consent, legal/refunds).
 */
export const setAddon = (workspace: Workspace, addon: Addon) =>
  api<BillingWorkspace>(`${billingPath(workspace)}/addon`, {
    method: "PUT",
    headers: { "content-type": "application/json", "idempotency-key": idempotencyKey() },
    body: JSON.stringify({ addon, accept_refund_policy: addon !== "" }),
  })

/**
 * Top-up size rules (user decision 2026-10-18): multiples of 100 GB, at most 10,000 GB per
 * purchase. The server enforces them and publishes them on the top-up product as `unit_bytes`
 * and `max_quantity`; these values only shape the form until that product is served.
 */
export const topupDefaults = { unitBytes: 100e9, maxQuantity: 100 } as const

/** Checkout for `quantity` units of the top-up product (each `unit_bytes`, priced `unit_amount`). */
export async function buyTopup(workspace: Workspace, product: BillingProduct, quantity: number, key: string, termsVersion: string) {
  const back = `${window.location.origin}/billing`
  const { url } = (await post(
    `${billingPath(workspace)}/topups`,
    {
      product_code: product.code,
      product_version: product.version,
      quantity,
      success_url: `${back}?checkout=done`,
      cancel_url: back,
      terms_version: termsVersion,
    },
    key,
  )) as { url: string }
  return url
}

/** "$8", "$15.50" from minor units. */
export function formatMoney(amount: number, currency: string) {
  return new Intl.NumberFormat(undefined, {
    style: "currency",
    currency: currency.toUpperCase(),
    minimumFractionDigits: amount % 100 ? 2 : 0,
  }).format(amount / 100)
}
