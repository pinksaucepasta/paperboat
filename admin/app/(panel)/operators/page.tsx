"use client"

import { DataTable } from "@/components/data-table"
import { Mono } from "@/components/filters"
import { CreateOperator, OperatorRowActions, OperatorStatus, operatorMfa } from "@/components/operators"
import { Page } from "@/components/page"
import { Time } from "@/components/time"
import { Badge } from "@/components/ui/badge"
import { useOperator, type Data } from "@/lib/api"
import { useCursorList } from "@/lib/paging"

const noFilters = {}

export default function OperatorsPage() {
  const me = useOperator()
  const list = useCursorList<Data<"/v1/admin/operators">>("/v1/admin/operators", noFilters)
  return (
    <Page title="Operators" description="Who can sign in to this panel and the admin API. Every change here is audited." actions={me?.role === "admin" ? <CreateOperator /> : undefined}>
      <DataTable
        what="operators"
        list={list}
        rows={(d) => d.operators}
        rowKey={(o) => o.id}
        empty="No operators."
        columns={[
          { header: "Email", cell: (o) => <span className="flex items-center gap-2"><Mono>{o.email}</Mono>{o.email === me?.email && <Badge variant="outline">You</Badge>}</span> },
          { header: "Role", cell: (o) => o.role },
          { header: "Status", cell: (o) => <OperatorStatus op={o} /> },
          { header: "Authenticator", cell: (o) => operatorMfa(o) },
          { header: "Added", cell: (o) => <Time iso={o.created_at} relative /> },
          { header: "", cell: (o) => (o.email === me?.email ? null : <OperatorRowActions op={o} />) },
        ]}
      />
    </Page>
  )
}
