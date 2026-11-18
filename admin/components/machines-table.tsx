"use client"

import Link from "next/link"
import { DataTable } from "@/components/data-table"
import { Mono, StatusBadge } from "@/components/filters"
import { Time } from "@/components/time"
import type { Data } from "@/lib/api"
import type { CursorList } from "@/lib/paging"

type Machines = Data<"/v1/admin/machines">

export function MachinesTable({ list, empty }: { list: CursorList<Machines>; empty: string }) {
  return (
    <DataTable
      what="machines"
      list={list}
      rows={(d) => d.machines}
      rowKey={(m) => m.id}
      empty={empty}
      columns={[
        { header: "Machine", cell: (m) => <Link className="font-medium underline-offset-2 hover:underline" href={`/machines/${encodeURIComponent(m.id)}`}>{m.alias}</Link> },
        { header: "State", cell: (m) => <StatusBadge state={m.deleted_at ? "removed" : m.stale ? "stale" : m.state} /> },
        { header: "Platform", className: "hidden md:table-cell", cell: (m) => `${m.platform}/${m.architecture}` },
        { header: "Owner", className: "hidden md:table-cell", cell: (m) => <Mono>{m.team_id || m.account_id}</Mono> },
        { header: "Seen", cell: (m) => (m.last_seen_at ? <Time iso={m.last_seen_at} relative /> : "Never") },
      ]}
    />
  )
}
