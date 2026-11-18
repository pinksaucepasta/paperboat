"use client"

import { DataTable } from "@/components/data-table"
import { CreateOperator, OperatorRowActions, OperatorStatus, operatorMfa } from "@/components/operators"
import { Page } from "@/components/page"
import { Time } from "@/components/time"
import { useOperator, type Data } from "@/lib/api"
import { useCursorList } from "@/lib/paging"

const noFilters = {}

export default function OperatorsPage() {
  const me = useOperator()
  const list = useCursorList<Data<"/v1/admin/operators">>("/v1/admin/operators", noFilters)
  return (
    <Page title="Operators">
      {me?.role === "admin" && (
        <div className="flex justify-end">
          <CreateOperator />
        </div>
      )}
      <DataTable
        what="operators"
        list={list}
        rows={(d) => d.operators}
        rowKey={(o) => o.id}
        empty="No operators."
        columns={[
          { header: "Email", cell: (o) => <span className="flex items-center gap-2"><span className="font-medium">{o.email}</span>{o.email === me?.email && <span className="text-muted-foreground">You</span>}</span> },
          { header: "Role", cell: (o) => <span className="inline-block first-letter:uppercase">{o.role}</span> },
          { header: "Status", cell: (o) => <OperatorStatus op={o} /> },
          { header: "MFA", cell: (o) => operatorMfa(o) },
          { header: "Sessions", cell: (o) => <span data-live-sessions={o.live_sessions}>{o.live_sessions}</span>, className: "tabular-nums" },
          { header: "Added", cell: (o) => <Time iso={o.created_at} relative /> },
          { header: "", className: "w-12", cell: (o) => (o.email === me?.email ? null : <OperatorRowActions op={o} />) },
        ]}
      />
    </Page>
  )
}
