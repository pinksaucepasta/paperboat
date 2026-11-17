"use client"

import { useMemo } from "react"
import Link from "next/link"
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
    <Page title="Sentry issues" description="Recent issues from the configured Sentry projects. Sentry does not report a total, so this list pages without one.">
      <DataTable
        what="Sentry issues"
        list={list}
        rows={(d) => d.issues}
        rowKey={(i) => `${i.project}/${i.id}`}
        empty="Sentry has no issues to show."
        columns={[
          { header: "Issue", cell: (i) => <Mono>{i.short_id ?? i.id}</Mono> },
          { header: "Project", cell: (i) => i.project },
          { header: "Level", cell: (i) => i.level ?? "—" },
          { header: "Status", cell: (i) => <StatusBadge state={i.status} /> },
          { header: "Events", cell: (i) => i.count, className: "text-right tabular-nums" },
          { header: "Last seen", cell: (i) => <Time iso={i.last_seen} relative /> },
        ]}
      />
      <p className="text-sm text-muted-foreground">
        Have a reference from a customer? Use <Link className="underline" href="/support">Support lookup</Link>.
      </p>
    </Page>
  )
}
