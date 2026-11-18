"use client"

import { useMemo } from "react"
import { DataTable } from "@/components/data-table"
import { Mono, StatusBadge } from "@/components/filters"
import { Page } from "@/components/page"
import { Time } from "@/components/time"
import type { Data } from "@/lib/api"
import { useCursorList } from "@/lib/paging"

export default function SentryPage() {
  const filters = useMemo(() => ({}), [])
  const list = useCursorList<Data<"/v1/admin/sentry/issues">>("/v1/admin/sentry/issues", filters, "cursor")
  return (
    <Page title="Errors">
      <DataTable
        what="errors"
        list={list}
        rows={(d) => d.issues}
        rowKey={(i) => `${i.project}/${i.id}`}
        empty="No errors."
        columns={[
          { header: "Issue", cell: (i) => <Mono>{i.short_id ?? i.id}</Mono> },
          { header: "Project", className: "hidden md:table-cell", cell: (i) => i.project },
          { header: "Level", className: "hidden md:table-cell", cell: (i) => i.level ?? "—" },
          { header: "Status", cell: (i) => <StatusBadge state={i.status} /> },
          { header: "Events", cell: (i) => i.count, className: "text-right tabular-nums" },
          { header: "Last seen", cell: (i) => <Time iso={i.last_seen} relative /> },
        ]}
      />
    </Page>
  )
}
