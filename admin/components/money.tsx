"use client"

import Link from "next/link"
import { useState } from "react"
import { Delta } from "@/components/analytics"
import { DataTable, type Column } from "@/components/data-table"
import { Mono } from "@/components/filters"
import { Panel, Stat } from "@/components/page"
import { Time } from "@/components/time"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import type { Data } from "@/lib/api"
import { formatMoney } from "@/lib/format"
import { useCursorList } from "@/lib/paging"

export type MoneyReport = Data<"/v1/admin/money">
export type MoneyRow = MoneyReport["customers"]["rows"][number]
export type ListKind = "customers" | "past_due" | "renewals" | "trials"

const when = (unix?: number) => (unix ? <Time iso={new Date(unix * 1000).toISOString()} relative /> : "—")

/** The one place a customer links to the admin: its billing workspace. A Stripe customer we have no record of shows its id. */
function Who({ row }: { row: MoneyRow }) {
  return row.workspace ? (
    <Link href={`/billing?workspace=${encodeURIComponent(row.workspace)}`} className="underline-offset-4 hover:underline">
      <Mono>{row.workspace}</Mono>
    </Link>
  ) : (
    <span className="text-muted-foreground"><Mono>{row.customer}</Mono> · no workspace</span>
  )
}

/** What each list shows (the API's field meanings, in words). */
export function columnsFor(kind: ListKind, money: (n: number) => string): Column<MoneyRow>[] {
  const who: Column<MoneyRow> = { header: "Workspace", cell: (r) => <Who row={r} /> }
  const plan: Column<MoneyRow> = { header: "Plan", className: "hidden sm:table-cell", cell: (r) => r.plan ?? "—" }
  switch (kind) {
    case "customers":
      return [who, plan, { header: "Seats", className: "hidden sm:table-cell text-right tabular-nums", cell: (r) => r.seats }, { header: "MRR", className: "text-right tabular-nums", cell: (r) => money(r.mrr) }]
    case "past_due":
      return [who, { header: "Owed", className: "text-right tabular-nums", cell: (r) => (r.amount ? money(r.amount) : "—") }, { header: "Tries", className: "hidden sm:table-cell text-right tabular-nums", cell: (r) => r.attempts ?? 0 }, { header: "Next retry", cell: (r) => (r.state === "unpaid" ? "Stopped" : when(r.at)) }]
    case "renewals":
      return [who, plan, { header: "Charge", className: "text-right tabular-nums", cell: (r) => money(r.amount ?? 0) }, { header: "Renews", cell: (r) => when(r.at) }]
    case "trials":
      return [who, plan, { header: "Becomes", className: "text-right tabular-nums", cell: (r) => `${money(r.mrr)}/mo` }, { header: "Ends", cell: (r) => when(r.at) }]
  }
}

/** A figure with its change against the range before (omitted when Stripe cannot say). */
export function MoneyStat({ label, value, previous, worseUp, points, note }: { label: string; value: string; previous?: { now: number; before: number }; worseUp?: boolean; points?: boolean; note?: React.ReactNode }) {
  return (
    <Stat
      label={label}
      value={value}
      note={
        <span className="flex flex-wrap gap-x-2">
          {previous && <Delta value={previous.now} previous={previous.before} worseUp={worseUp} points={points} />}
          {note}
        </span>
      }
    />
  )
}

function Rows({ rows, columns }: { rows: MoneyRow[]; columns: Column<MoneyRow>[] }) {
  return (
    <table className="w-full border-collapse text-sm">
      <thead>
        <tr className="border-b border-divider text-left text-xs text-muted-foreground">
          {columns.map((c) => (
            <th key={c.header} scope="col" className={`h-8 font-normal ${c.className ?? ""}`}>{c.header}</th>
          ))}
        </tr>
      </thead>
      <tbody className="divide-y divide-divider">
        {rows.map((row, i) => (
          <tr key={`${row.customer}-${i}`} className="[&>td]:h-11 [&>td]:pr-3">
            {columns.map((c) => (
              <td key={c.header} className={c.className}>{c.cell(row)}</td>
            ))}
          </tr>
        ))}
      </tbody>
    </table>
  )
}

function AllRows({ kind, columns }: { kind: ListKind; columns: Column<MoneyRow>[] }) {
  const [filters] = useState({})
  const list = useCursorList<Data<"/v1/admin/money/lists/{list}">>(`/v1/admin/money/lists/${kind}`, filters)
  return <DataTable what="the list" list={list} rows={(d) => d.rows} rowKey={(r) => `${r.customer}-${r.at}-${r.mrr}`} empty="Nothing here." columns={columns} />
}

/** A card of at most five rows; "View all" opens the whole list, paged. */
export function ListCard({ title, kind, total, rows, money, empty, summary }: { title: string; kind: ListKind; total: number; rows: MoneyRow[]; money: (n: number) => string; empty: string; summary?: React.ReactNode }) {
  const [open, setOpen] = useState(false)
  const columns = columnsFor(kind, money)
  return (
    <Panel title={title} action={total > rows.length ? <Button variant="outline" size="sm" onClick={() => setOpen(true)}>View all {total}</Button> : undefined}>
      <div data-money-list={kind}>
        {summary && <p className="pb-2 text-sm text-muted-foreground">{summary}</p>}
        {rows.length === 0 ? <p className="py-6 text-center text-sm text-muted-foreground">{empty}</p> : <Rows rows={rows} columns={columns} />}
      </div>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent className="max-h-[90svh] overflow-y-auto sm:max-w-3xl">
          <DialogHeader>
            <DialogTitle>{title}</DialogTitle>
          </DialogHeader>
          {open && <AllRows kind={kind} columns={columns} />}
        </DialogContent>
      </Dialog>
    </Panel>
  )
}

export const moneyFormatter = (currency: string) => (n: number) => formatMoney(n, currency)
