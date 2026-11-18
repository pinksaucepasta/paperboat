"use client"

import { DataTable } from "@/components/data-table"
import { Mono } from "@/components/filters"
import { NoticeDialog } from "@/components/notices"
import { Page } from "@/components/page"
import { Time } from "@/components/time"
import { useOperator, type Data } from "@/lib/api"
import { useCursorList } from "@/lib/paging"

const kindLabel: Record<string, string> = { price_increase: "Price increase", terms_change: "Terms change", subprocessor: "New subprocessor", breach: "Data breach" }
const none = {}

export default function NoticesPage() {
  const admin = useOperator()?.role === "admin"
  const list = useCursorList<Data<"/v1/admin/notices">>("/v1/admin/notices", none)
  return (
    <Page title="Customer notices">
      <div className="flex justify-end">{admin && <NoticeDialog />}</div>
      <DataTable
        what="notices"
        list={list}
        rows={(d) => d.notices}
        rowKey={(n) => n.id!}
        empty="No notices sent."
        columns={[
          { header: "Notice", cell: (n) => (
            <span className="flex min-w-0 flex-col">
              <span className="truncate font-medium">{n.subject}</span>
              <span className="text-xs text-muted-foreground">{kindLabel[n.kind] ?? n.kind}{n.late ? " · sent late" : ""}</span>
            </span>
          ) },
          { header: "Audience", className: "hidden md:table-cell", cell: (n) => <Mono>{n.audience_value ? `${n.audience_kind}: ${n.audience_value}` : n.audience_kind}</Mono> },
          { header: "Recipients", cell: (n) => <span className="tabular-nums">{n.recipients}</span> },
          { header: "Sent", className: "hidden md:table-cell", cell: (n) => (n.created_at ? <Time iso={n.created_at} /> : null) },
        ]}
      />
    </Page>
  )
}
