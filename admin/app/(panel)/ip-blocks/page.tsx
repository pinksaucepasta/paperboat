"use client"

import { useMemo, useState } from "react"
import { DataTable } from "@/components/data-table"
import { Filters, Mono } from "@/components/filters"
import { BlockNetwork, IPBlockState, blockEnd, blockRange } from "@/components/ip-blocks"
import { Page } from "@/components/page"
import { Time } from "@/components/time"
import { useOperator, type Data } from "@/lib/api"
import { useCursorList } from "@/lib/paging"

export default function IPBlocksPage() {
  const admin = useOperator()?.role === "admin"
  const [state, setState] = useState("")
  const filters = useMemo(() => ({ state }), [state])
  const list = useCursorList<Data<"/v1/admin/ip-blocks">>("/v1/admin/ip-blocks", filters)
  return (
    <Page title="IP blocks">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <Filters items={[{ label: "State", value: state, onChange: setState, options: [["running", "Running"], ["ended", "Lifted or expired"]] }]} />
        {admin && <BlockNetwork />}
      </div>
      <DataTable
        what="IP blocks"
        list={list}
        rows={(d) => d.blocks}
        rowKey={(b) => b.id}
        empty="No networks are blocked."
        columns={[
          { header: "Network", cell: (b) => blockRange(b) },
          { header: "Reference", cell: (b) => <Mono>{b.reason}</Mono> },
          { header: "Ends", className: "hidden md:table-cell", cell: (b) => blockEnd(b) },
          { header: "Added", className: "hidden lg:table-cell", cell: (b) => <span className="text-muted-foreground"><Time iso={b.created_at} relative /> · {b.created_by}</span> },
          { header: "State", cell: (b) => <IPBlockState block={b} /> },
        ]}
      />
    </Page>
  )
}
