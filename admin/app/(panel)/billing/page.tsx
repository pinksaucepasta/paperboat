"use client"

import { Suspense, useMemo, useState } from "react"
import { useSearchParams } from "next/navigation"
import { BillingPolicy, OperationActions, ProductEditor } from "@/components/billing-actions"
import { DataTable } from "@/components/data-table"
import { FilterSelect, Mono, StatusBadge } from "@/components/filters"
import { Page } from "@/components/page"
import { Tabs } from "@/components/tabs"
import { Time } from "@/components/time"
import { useOperator, type Data } from "@/lib/api"
import { useCursorList } from "@/lib/paging"

type View = "workspaces" | "operations" | "products"
const billingStates: [string, string][] = [["", "Any state"], ...["free", "trialing", "active", "past_due", "unpaid", "canceled", "paused", "incomplete"].map((s): [string, string] => [s, s.replace("_", " ")])]

export default function BillingPage() {
  const [view, setView] = useState<View>("workspaces")
  return (
    <Page title="Billing & plans" description="Billing workspaces, recovery operations and the product catalog.">
      <Tabs value={view} onChange={setView} options={[["workspaces", "Workspaces"], ["operations", "Operations"], ["products", "Plans & prices"]]} />
      <Suspense>
        {view === "workspaces" && <Workspaces />}
        {view === "operations" && <Operations />}
        {view === "products" && <Products />}
      </Suspense>
    </Page>
  )
}

function Workspaces() {
  const [state, setState] = useState("")
  const highlight = useSearchParams().get("workspace") ?? ""
  const filters = useMemo(() => ({ state }), [state])
  const list = useCursorList<Data<"/v1/admin/billing/workspaces">>("/v1/admin/billing/workspaces", filters)
  return (
    <>
      <FilterSelect label="State" value={state} onChange={setState} options={billingStates} />
      <DataTable
        what="billing workspaces"
        list={list}
        rows={(d) => d.workspaces}
        rowKey={(w) => w.id}
        empty="No billing workspace matches this state."
        columns={[
          { header: "Workspace", cell: (w) => <span className={w.id === highlight ? "font-medium underline" : undefined}><Mono>{w.id}</Mono></span> },
          { header: "Owner", cell: (w) => <Mono>{`${w.scope}: ${w.scope_id}`}</Mono> },
          { header: "Plan", cell: (w) => w.product_code ?? "—" },
          { header: "State", cell: (w) => <StatusBadge state={w.state} /> },
          { header: "Seats", cell: (w) => w.quantity, className: "text-right tabular-nums" },
          { header: "Period ends", cell: (w) => (w.period_end ? <Time iso={w.period_end} /> : "—") },
        ]}
      />
    </>
  )
}

function Operations() {
  const [state, setState] = useState("")
  const filters = useMemo(() => ({ state }), [state])
  const list = useCursorList<Data<"/v1/admin/billing/workspace-operations">>("/v1/admin/billing/workspace-operations", filters)
  return (
    <>
      <FilterSelect label="Operation state" value={state} onChange={setState} options={[["", "Any state"], ["pending", "Pending"], ["succeeded", "Succeeded"], ["failed", "Failed"], ["uncertain", "Uncertain"]]} />
      <DataTable
        what="billing operations"
        list={list}
        rows={(d) => d.operations}
        rowKey={(o) => o.id}
        empty="No billing operation matches this state."
        columns={[
          { header: "Operation", cell: (o) => <Mono>{o.id}</Mono> },
          { header: "Kind", cell: (o) => o.kind },
          { header: "Workspace", cell: (o) => <Mono>{o.workspace_id}</Mono> },
          { header: "State", cell: (o) => <StatusBadge state={o.state} /> },
          { header: "Updated", cell: (o) => <Time iso={o.updated_at} relative /> },
          { header: "Recovery", cell: (o) => <OperationActions id={o.id} state={o.state} /> },
        ]}
      />
    </>
  )
}

function Products() {
  const filters = useMemo(() => ({}), [])
  const list = useCursorList<Data<"/v1/admin/billing/products">>("/v1/admin/billing/products", filters)
  const admin = useOperator()?.role === "admin"
  return (
    <div className="flex flex-col gap-4">
      <BillingPolicy />
      {admin && <div><ProductEditor /></div>}
      <DataTable
        what="plans and prices"
        list={list}
        rows={(d) => d.products}
        rowKey={(p) => p.code}
        empty="The catalog has no products."
        columns={[
          { header: "Code", cell: (p) => <Mono>{p.code}</Mono> },
          { header: "Name", cell: (p) => p.display_name },
          { header: "Type", cell: (p) => p.catalog_type },
          { header: "Price", cell: (p) => `${(p.unit_amount / 100).toFixed(2)} ${p.currency.toUpperCase()}${p.interval ? ` / ${p.interval}` : ""}` },
          { header: "Active", cell: (p) => <StatusBadge state={p.active ? "active" : "revoked"} /> },
          { header: "Price verified", cell: (p) => (p.price_verified ? "yes" : "no") },
          { header: "Actions", cell: (p) => (admin ? <ProductEditor key={`${p.code}-${p.version}`} product={p} /> : "—") },
        ]}
      />
    </div>
  )
}
