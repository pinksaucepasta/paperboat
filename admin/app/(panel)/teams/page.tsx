"use client"

import Link from "next/link"
import { useMemo, useState } from "react"
import { DataTable } from "@/components/data-table"
import { FilterSelect, Mono, SearchBox, StatusBadge } from "@/components/filters"
import { Page } from "@/components/page"
import type { Data } from "@/lib/api"
import { useCursorList } from "@/lib/paging"

export default function TeamsPage() {
  const [q, setQ] = useState("")
  const [suspended, setSuspended] = useState("")
  const filters = useMemo(() => ({ q, suspended }), [q, suspended])
  const list = useCursorList<Data<"/v1/admin/teams">>("/v1/admin/teams", filters)
  return (
    <Page title="Teams" description="Teams and their billing state. Search by team id or owner account id.">
      <div className="flex flex-wrap items-center gap-2">
        <SearchBox label="Search teams" placeholder="Team or owner id" value={q} onChange={setQ} />
        <FilterSelect label="Suspension" value={suspended} onChange={setSuspended} options={[["", "Any"], ["false", "Not suspended"], ["true", "Suspended"]]} />
      </div>
      <DataTable
        what="teams"
        list={list}
        rows={(d) => d.teams}
        rowKey={(t) => t.id}
        empty="No team matches these filters."
        columns={[
          { header: "Team", cell: (t) => <Link className="underline-offset-2 hover:underline" href={`/teams/${encodeURIComponent(t.id)}`}><Mono>{t.id}</Mono></Link> },
          { header: "Owner", cell: (t) => <Link className="underline-offset-2 hover:underline" href={`/users/${encodeURIComponent(t.owner_account)}`}><Mono>{t.owner_account}</Mono></Link> },
          { header: "Status", cell: (t) => <StatusBadge state={t.suspended ? "suspended" : "active"} /> },
          { header: "Billing", cell: (t) => (t.billing_state ? <StatusBadge state={t.billing_state} /> : "—") },
        ]}
      />
    </Page>
  )
}
