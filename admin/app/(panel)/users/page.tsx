"use client"

import Link from "next/link"
import { useMemo, useState } from "react"
import { DataTable } from "@/components/data-table"
import { FilterSelect, Mono, SearchBox, StatusBadge } from "@/components/filters"
import { Page } from "@/components/page"
import { Time } from "@/components/time"
import type { Data } from "@/lib/api"
import { useCursorList } from "@/lib/paging"

type Accounts = Data<"/v1/admin/accounts">

export default function UsersPage() {
  const [q, setQ] = useState("")
  const [status, setStatus] = useState("")
  const filters = useMemo(() => ({ q, status }), [q, status])
  const list = useCursorList<Accounts>("/v1/admin/accounts", filters)
  return (
    <Page title="Users" description="Customer accounts. Search by email, name or account id.">
      <div className="flex flex-wrap items-center gap-2">
        <SearchBox label="Search users" placeholder="Email, name or id" value={q} onChange={setQ} />
        <FilterSelect label="Status" value={status} onChange={setStatus} options={[["", "Any status"], ["active", "Active"], ["suspended", "Suspended"]]} />
      </div>
      <DataTable
        what="users"
        list={list}
        rows={(d) => d.accounts}
        rowKey={(a) => a.id}
        empty="No account matches these filters."
        columns={[
          { header: "Email", cell: (a) => <Link className="underline-offset-2 hover:underline" href={`/users/${encodeURIComponent(a.id)}`}>{a.primary_email}</Link> },
          { header: "Name", cell: (a) => a.display_name },
          { header: "Status", cell: (a) => <StatusBadge state={a.status} /> },
          { header: "Account id", cell: (a) => <Mono>{a.id}</Mono> },
          { header: "Created", cell: (a) => <Time iso={a.created_at} /> },
        ]}
      />
    </Page>
  )
}
