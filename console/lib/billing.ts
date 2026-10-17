"use client"

import { queryOptions } from "@tanstack/react-query"
import { api, type Workspace } from "@/lib/api"
import type { components } from "@/lib/api-schema"

export type BillingWorkspace = components["schemas"]["BillingWorkspace"]
export type BillingProduct = components["schemas"]["WorkspaceBillingProduct"]
export type BillingInterval = "month" | "year"

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
  return new Intl.NumberFormat(undefined, {
    style: "currency",
    currency: product.currency.toUpperCase(),
    minimumFractionDigits: product.unit_amount % 100 ? 2 : 0,
  }).format(product.unit_amount / 100)
}
