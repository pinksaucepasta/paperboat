"use client"

import { DownloadSimpleIcon } from "@phosphor-icons/react"
import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query"
import { useMemo, useState } from "react"
import { ApiError, idempotencyKey, useApiQuery, type Workspace } from "@/lib/api"
import {
  billingDetailsQuery,
  formatMoney,
  invoicesQuery,
  payByInvoiceQuery,
  saveBillingDetails,
  setPayByInvoice,
  type BillingDetails,
  type BillingInvoice,
  type BillingWorkspace,
} from "@/lib/billing"
import { useTimeFormat } from "@/lib/use-time-format"
import { Row, Section } from "@/components/section"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { Switch } from "@/components/ui/switch"

/*
 * Invoices, billing details and pay by invoice (server/docs/teams-business.md). Shown to whoever
 * may manage this workspace's billing (`can_manage`: a personal workspace's owner, a team's owner
 * and billing admins). Stripe keeps the invoices and details; the console only shows and edits them.
 */

const statusLabels: Record<string, string> = { paid: "Paid", open: "Due", void: "Void", uncollectible: "Unpaid" }

export function Invoices({ workspace }: { workspace: Workspace }) {
  const time = useTimeFormat()
  const invoices = useInfiniteQuery(invoicesQuery(workspace))
  const items = invoices.data?.pages.flatMap((page) => page.items) ?? []

  return (
    <Section title="Invoices">
      {invoices.isPending ? (
        <Skeleton className="mt-2 h-24 w-full" />
      ) : invoices.isError ? (
        <p className="py-3 text-sm text-muted-foreground">{invoices.error.message}</p>
      ) : !items.length ? (
        <p className="py-3 text-sm text-muted-foreground">No invoices yet. They appear here after the first payment.</p>
      ) : (
        <table className="w-full text-sm">
          <thead className="text-left text-xs text-muted-foreground">
            <tr className="border-b border-divider [&>th]:h-9 [&>th]:font-normal">
              <th>Date</th>
              <th className="hidden sm:table-cell">Number</th>
              <th className="hidden md:table-cell">Period</th>
              <th className="text-right">Total</th>
              <th className="pl-4">Status</th>
              <th aria-label="Download" />
            </tr>
          </thead>
          <tbody className="divide-y divide-divider border-b border-divider">
            {items.map((invoice) => (
              <InvoiceRow key={invoice.id} invoice={invoice} date={time.date} />
            ))}
          </tbody>
        </table>
      )}
      {invoices.hasNextPage && (
        <Button
          variant="outline"
          className="mt-3 self-start"
          disabled={invoices.isFetchingNextPage}
          onClick={() => invoices.fetchNextPage()}
        >
          {invoices.isFetchingNextPage ? "Loading…" : "Show older invoices"}
        </Button>
      )}
    </Section>
  )
}

function InvoiceRow({
  invoice,
  date,
}: {
  invoice: BillingInvoice
  date: (iso: string, style: "date" | "day") => string
}) {
  const due = invoice.status === "open"
  return (
    <tr className="[&>td]:h-12">
      <td>{date(invoice.created_at, "date")}</td>
      <td className="hidden font-mono text-code text-muted-foreground sm:table-cell">{invoice.number}</td>
      <td className="hidden text-muted-foreground md:table-cell">
        {date(invoice.period_start, "day")} – {date(invoice.period_end, "date")}
      </td>
      <td className="text-right tabular-nums">
        {formatMoney(invoice.total, invoice.currency)}
        {invoice.tax > 0 && (
          <span className="block text-xs text-muted-foreground">
            incl. {formatMoney(invoice.tax, invoice.currency)} tax
          </span>
        )}
      </td>
      <td className="pl-4">
        <Badge variant={due ? "destructive" : "outline"} className="font-normal">
          {statusLabels[invoice.status] ?? invoice.status}
        </Badge>
        {due && invoice.due_date && (
          <span className="ml-2 hidden text-xs text-muted-foreground lg:inline">by {date(invoice.due_date, "date")}</span>
        )}
      </td>
      <td className="text-right whitespace-nowrap">
        {invoice.hosted_url && (
          <Button variant="ghost" size="sm" nativeButton={false} render={<a href={invoice.hosted_url} target="_blank" rel="noopener" />}>
            {due ? "Pay" : "View"}
          </Button>
        )}
        {invoice.pdf_url && (
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label={`Download invoice ${invoice.number} as PDF`}
            nativeButton={false}
            render={<a href={invoice.pdf_url} target="_blank" rel="noopener" />}
          >
            <DownloadSimpleIcon />
          </Button>
        )}
      </td>
    </tr>
  )
}

/** Common Stripe tax ID types; the server accepts any Stripe type. */
const taxTypes: Record<string, string> = {
  eu_vat: "EU VAT",
  gb_vat: "UK VAT",
  ch_vat: "Swiss VAT",
  no_vat: "Norwegian VAT",
  au_abn: "Australian ABN",
  ca_bn: "Canadian BN",
  in_gst: "Indian GST",
  sg_gst: "Singapore GST",
  jp_cn: "Japanese corporate number",
  us_ein: "US EIN",
}

type Draft = {
  email: string
  name: string
  line1: string
  line2: string
  city: string
  state: string
  postal_code: string
  country: string
  taxType: string
  taxValue: string
}

const draftOf = (details: BillingDetails): Draft => ({
  email: details.email,
  name: details.name,
  ...details.address,
  taxType: details.tax_id?.type ?? "",
  taxValue: details.tax_id?.value ?? "",
})

/** Printed on every later invoice; the email receives invoices and receipts. */
export function BillingDetailsSection({ workspace }: { workspace: Workspace }) {
  const details = useApiQuery(billingDetailsQuery(workspace))
  return (
    <Section title="Billing details">
      {details.isPending ? (
        <Skeleton className="mt-2 h-40 w-full" />
      ) : details.isError ? (
        <p className="py-3 text-sm text-muted-foreground">
          {details.error instanceof ApiError && details.error.code === "no_billing_account"
            ? "Billing details can be added after the first payment."
            : details.error.message}
        </p>
      ) : (
        <DetailsForm key={JSON.stringify(details.data)} workspace={workspace} saved={details.data} />
      )}
    </Section>
  )
}

function DetailsForm({ workspace, saved }: { workspace: Workspace; saved: BillingDetails }) {
  const queryClient = useQueryClient()
  const initial = useMemo(() => draftOf(saved), [saved])
  const [draft, setDraft] = useState(initial)
  const changed = JSON.stringify(draft) !== JSON.stringify(initial)
  // One key per distinct change, so a retry of the same form cannot apply twice.
  const key = useMemo(() => idempotencyKey(), [draft]) // eslint-disable-line react-hooks/exhaustive-deps
  const field = (name: keyof Draft) => ({
    id: `billing-${name}`,
    value: draft[name],
    onChange: (event: React.ChangeEvent<HTMLInputElement>) => setDraft({ ...draft, [name]: event.target.value }),
  })
  const save = useMutation({
    mutationFn: () =>
      saveBillingDetails(
        workspace,
        {
          email: draft.email.trim(),
          name: draft.name.trim(),
          address: {
            line1: draft.line1.trim(),
            line2: draft.line2.trim(),
            city: draft.city.trim(),
            state: draft.state.trim(),
            postal_code: draft.postal_code.trim(),
            country: draft.country.trim().toUpperCase(),
          },
          tax_id: draft.taxType && draft.taxValue.trim() ? { type: draft.taxType, value: draft.taxValue.trim() } : null,
        },
        key,
      ),
    onSuccess: (next) => queryClient.setQueryData(billingDetailsQuery(workspace).queryKey, next),
  })
  const verification = saved.tax_id?.verification

  return (
    <form
      className="flex flex-col"
      onSubmit={(event) => {
        event.preventDefault()
        if (changed) save.mutate()
      }}
    >
      <Row label="Billing email" htmlFor="billing-email">
        <Input type="email" required className="w-72 max-w-full" {...field("email")} />
      </Row>
      <Row label="Name on invoices" htmlFor="billing-name">
        <Input placeholder="Company or person" className="w-72 max-w-full" {...field("name")} />
      </Row>
      <div className="flex flex-wrap items-start justify-between gap-6 border-b border-divider py-3 text-sm">
        <label htmlFor="billing-line1" className="pt-2 text-muted-foreground">
          Address
        </label>
        <div className="grid w-72 max-w-full gap-2">
          <Input placeholder="Street and number" {...field("line1")} />
          <Input placeholder="Apartment, suite (optional)" {...field("line2")} />
          <div className="grid grid-cols-2 gap-2">
            <Input placeholder="City" {...field("city")} />
            <Input placeholder="Postal code" {...field("postal_code")} />
          </div>
          <div className="grid grid-cols-2 gap-2">
            <Input placeholder="State or region" {...field("state")} />
            <Input
              placeholder="Country (e.g. DE)"
              maxLength={2}
              aria-label="Country code"
              {...field("country")}
            />
          </div>
        </div>
      </div>
      <div className="flex flex-wrap items-start justify-between gap-6 border-b border-divider py-3 text-sm">
        <label htmlFor="billing-taxValue" className="pt-2 text-muted-foreground">
          Tax ID
        </label>
        <div className="grid w-72 max-w-full gap-2">
          <select
            aria-label="Tax ID type"
            className="h-8 rounded-md border border-input bg-transparent px-2 text-sm"
            value={draft.taxType}
            onChange={(event) => setDraft({ ...draft, taxType: event.target.value })}
          >
            <option value="">None</option>
            {Object.entries(taxTypes).map(([type, label]) => (
              <option key={type} value={type}>
                {label}
              </option>
            ))}
            {draft.taxType && !taxTypes[draft.taxType] && <option value={draft.taxType}>{draft.taxType}</option>}
          </select>
          {draft.taxType && <Input placeholder="Number" {...field("taxValue")} />}
          {verification && !changed && (
            <span className="text-xs text-muted-foreground">
              {verification === "verified" ? "Verified by Stripe" : verification === "pending" ? "Being verified" : "Couldn’t be verified"}
            </span>
          )}
        </div>
      </div>
      <div className="flex items-center justify-end gap-3 pt-3">
        {save.isError && <span className="text-sm text-destructive">{save.error.message}</span>}
        {save.isSuccess && !changed && <span className="text-sm text-muted-foreground">Saved. Later invoices use these details.</span>}
        <Button type="submit" disabled={!changed || save.isPending}>
          {save.isPending ? "Saving…" : "Save details"}
        </Button>
      </div>
    </form>
  )
}

/** Team only: Stripe emails each invoice to the billing email instead of charging the card. */
export function PayByInvoiceSection({ workspace, billing }: { workspace: Workspace; billing: BillingWorkspace }) {
  const queryClient = useQueryClient()
  const state = useApiQuery(payByInvoiceQuery(workspace))
  const change = useMutation({
    mutationFn: (on: boolean) => setPayByInvoice(workspace, on, idempotencyKey()),
    onSuccess: (next) => queryClient.setQueryData(payByInvoiceQuery(workspace).queryKey, next),
  })
  if (state.isPending) return null
  if (state.isError) return null
  const data = state.data
  const days = data.days_until_due ?? data.default_days_until_due
  return (
    <Section title="Pay by invoice">
      <div className="flex items-center justify-between gap-6 border-b border-divider py-3 text-sm">
        <div className="flex flex-col gap-0.5">
          <span>Pay by invoice instead of card</span>
          <span className="text-xs text-muted-foreground">
            {data.eligible || data.pay_by_invoice
              ? `Each invoice is emailed to the billing email and is due ${days} days after it is issued, payable by card or bank transfer. It applies from the next invoice.`
              : `Available on the yearly Team plan with ${data.min_seats} or more seats.`}
          </span>
          {change.isError && <span className="text-xs text-destructive">{change.error.message}</span>}
        </div>
        <Switch
          aria-label="Pay by invoice"
          checked={data.pay_by_invoice}
          disabled={!billing.can_manage || change.isPending || (!data.eligible && !data.pay_by_invoice)}
          onCheckedChange={(on) => change.mutate(on)}
        />
      </div>
    </Section>
  )
}
