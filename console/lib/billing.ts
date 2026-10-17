"use client"

import { queryOptions } from "@tanstack/react-query"
import { api, idempotencyKey, type Workspace } from "@/lib/api"
import type { components } from "@/lib/api-schema"

/**
 * Fields requested from Lane B (PLAN.md Log, 2026-10-18) and not served yet; each stays optional
 * and becomes part of the schema type when the server publishes it.
 */
type PendingBillingFields = {
  seats_bought?: number
  /** Members plus pending invitations: every invitation holds a seat. */
  seats_used?: number
  topup_bytes_remaining?: number
}
export type BillingWorkspace = components["schemas"]["BillingWorkspace"] & PendingBillingFields
/** Top-up products add `unit_bytes` and `max_quantity` (requested from Lane B). */
export type BillingProduct = components["schemas"]["WorkspaceBillingProduct"] & {
  unit_bytes?: number
  max_quantity?: number
}
export type BillingInterval = "month" | "year"

/**
 * Billing features whose endpoints the console may call. An unknown /v1 path answers 401, which
 * signs the user out (NOTES.md), so the UI for a feature is built but disabled until its endpoint
 * is deployed. Flip one when Lane B logs the deploy.
 */
export const billingLive = {
  seats: false,
  planChange: false,
  cancel: false,
  topups: false,
  paidTeamCreate: false,
} as const
export const notLiveTitle = "Not available yet"

/** What a change costs now and from the next invoice; Stripe prorates to the fixed billing date. */
export type ChangePreview = {
  amount_due_now: number
  currency: string
  next_invoice_amount: number
  next_invoice_at: string
}

const minute = 60_000

/** The workspace's billing routes: personal, or one team by ID. */
function billingPath(workspace: Workspace) {
  return workspace.kind === "team"
    ? `billing/workspaces/teams/${encodeURIComponent(workspace.id)}`
    : "billing/workspaces/personal"
}

/** The workspace's plan and its hosted relay + edge allowance for the current period. */
export const billingQuery = (workspace: Workspace | undefined) =>
  queryOptions({
    queryKey: ["billing", workspace?.id ?? ""],
    queryFn: () => api<BillingWorkspace>(billingPath(workspace!)),
    enabled: Boolean(workspace),
    staleTime: minute,
  })

/** Plans this workspace can buy, with verified prices. Personal sees Pro, a team sees Team. */
export const billingProductsQuery = (workspace: Workspace | undefined) =>
  queryOptions({
    queryKey: ["billing-products", workspace?.id ?? ""],
    queryFn: async () => (await api<{ items: BillingProduct[] }>(`${billingPath(workspace!)}/products`)).items,
    enabled: Boolean(workspace),
    staleTime: 5 * minute,
  })

/**
 * Starts Stripe Checkout and returns its URL. `key` is the operation's idempotency key: a retry
 * after an unknown provider outcome must send the same key so it cannot buy twice.
 */
export async function startCheckout(workspace: Workspace, product: BillingProduct, key: string) {
  const back = `${window.location.origin}/billing`
  const { url } = await api<{ url: string }>(`${billingPath(workspace)}/checkout`, {
    method: "POST",
    headers: { "content-type": "application/json", "idempotency-key": key },
    body: JSON.stringify({
      product_code: product.code,
      product_version: product.version,
      success_url: `${back}?checkout=done`,
      cancel_url: back,
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
    queryFn: () => api<ChangePreview>(`${billingPath(workspace)}/seats/preview?quantity=${quantity}`),
    enabled: billingLive.seats,
  })

/** Buys or removes seats. Added seats are invoiced now; removed ones stay paid until the billing date. */
export const setSeats = (workspace: Workspace, quantity: number) =>
  post(`${billingPath(workspace)}/seats`, { quantity })

export const planPreviewQuery = (workspace: Workspace, product: BillingProduct) =>
  queryOptions({
    queryKey: ["billing-plan-preview", workspace.id, product.code, product.version],
    queryFn: () =>
      api<ChangePreview>(
        `${billingPath(workspace)}/plan/preview?product_code=${encodeURIComponent(product.code)}&product_version=${product.version}`,
      ),
    enabled: billingLive.planChange,
  })

export const changePlan = (workspace: Workspace, product: BillingProduct) =>
  post(`${billingPath(workspace)}/plan`, { product_code: product.code, product_version: product.version })

/** Cancel keeps paid features until the period ends; resume undoes it before then. */
export const setCancelled = (workspace: Workspace, cancel: boolean) =>
  post(`${billingPath(workspace)}/${cancel ? "cancel" : "resume"}`, {})

/**
 * Top-up size rules (user decision 2026-10-18): multiples of 100 GB, at most 10,000 GB per
 * purchase. The server enforces them and publishes them on the top-up product as `unit_bytes`
 * and `max_quantity`; these values only shape the form until that product is served.
 */
export const topupDefaults = { unitBytes: 100e9, maxQuantity: 100 } as const

/** Checkout for `quantity` units of the top-up product (each `unit_bytes`, priced `unit_amount`). */
export async function buyTopup(workspace: Workspace, product: BillingProduct, quantity: number) {
  const back = `${window.location.origin}/billing`
  const { url } = (await post(`${billingPath(workspace)}/topups`, {
    product_code: product.code,
    product_version: product.version,
    quantity,
    success_url: `${back}?checkout=done`,
    cancel_url: back,
  })) as { url: string }
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
