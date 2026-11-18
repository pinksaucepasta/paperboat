"use client"

import { Suspense, useMemo, useState } from "react"
import { useSearchParams } from "next/navigation"
import { BillingPolicy, OperationActions, ProductEditor } from "@/components/billing-actions"
import { DataTable } from "@/components/data-table"
import { Filters, Mono, StatusBadge } from "@/components/filters"
import { Page, Panel } from "@/components/page"
import { Tabs } from "@/components/tabs"
import { Time } from "@/components/time"
import { useOperator, type Data } from "@/lib/api"
import { useCursorList } from "@/lib/paging"

type View = "workspaces" | "operations" | "products"
const billingStates: [string, string][] = ["free", "trialing", "active", "past_due", "unpaid", "canceled", "paused", "incomplete"].map((s) => [s, s[0].toUpperCase() + s.slice(1).replace("_", " ")])

export default function BillingPage() {
  const [view, setView] = useState<View>("workspaces")
  return (
    <Page title="Billing">
      <Tabs value={view} onChange={setView} options={[["workspaces", "Workspaces"], ["operations", "Operations"], ["products", "Plans"]]} />
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
      <div className="flex flex-wrap items-center gap-2"><Filters items={[{ label: "State", value: state, onChange: setState, options: billingStates }]} /></div>
      <DataTable
        what="billing workspaces"
        list={list}
        rows={(d) => d.workspaces}
        rowKey={(w) => w.id}
        empty="No workspaces match."
        columns={[
          { header: "Workspace", cell: (w) => <span className={w.id === highlight ? "rounded-sm bg-brand/10 px-1 py-0.5" : undefined}><Mono>{w.id}</Mono></span> },
          { header: "Owner", cell: (w) => <Mono>{`${w.scope}: ${w.scope_id}`}</Mono> },
          { header: "Plan", cell: (w) => w.product_code ?? "—" },
          { header: "State", cell: (w) => <StatusBadge state={w.state} /> },
          { header: "Seats", cell: (w) => w.quantity, className: "text-right tabular-nums" },
          { header: "Renews", cell: (w) => (w.period_end ? <Time iso={w.period_end} /> : "—") },
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
      <div className="flex flex-wrap items-center gap-2"><Filters items={[{ label: "Operation state", value: state, onChange: setState, options: [["pending", "Pending"], ["succeeded", "Succeeded"], ["failed", "Failed"], ["uncertain", "Uncertain"]] }]} /></div>
      <DataTable
        what="billing operations"
        list={list}
        rows={(d) => d.operations}
        rowKey={(o) => o.id}
        empty="No operations match."
        columns={[
          { header: "Operation", cell: (o) => <Mono>{o.id}</Mono> },
          { header: "Kind", cell: (o) => o.kind },
          { header: "Workspace", cell: (o) => <Mono>{o.workspace_id}</Mono> },
          { header: "State", cell: (o) => <StatusBadge state={o.state} /> },
          { header: "Updated", cell: (o) => <Time iso={o.updated_at} relative /> },
          { header: "", cell: (o) => <OperationActions id={o.id} state={o.state} /> },
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
    <div className="flex flex-col gap-8">
      <BillingPolicy />
      <Panel title="Plans" action={admin ? <ProductEditor /> : undefined}>
      <DataTable
        what="plans and prices"
        list={list}
        rows={(d) => d.products}
        rowKey={(p) => p.code}
        empty="No plans yet."
        columns={[
          { header: "Code", cell: (p) => <Mono>{p.code}</Mono> },
          { header: "Name", cell: (p) => p.display_name },
          { header: "Type", cell: (p) => p.catalog_type.replaceAll("_", " ") },
          { header: "Price", cell: (p) => `${(p.unit_amount / 100).toFixed(2)} ${p.currency.toUpperCase()}${p.interval ? ` / ${p.interval}` : ""}` },
          { header: "State", cell: (p) => <StatusBadge state={p.active ? "active" : "inactive"} /> },
          { header: "Verified", cell: (p) => (p.price_verified ? "Yes" : "No") },
          { header: "", className: "text-right", cell: (p) => (admin ? <ProductEditor key={`${p.code}-${p.version}`} product={p} /> : null) },
        ]}
      />
      </Panel>
    </div>
  )
}
