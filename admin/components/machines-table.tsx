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
        { header: "Machine", cell: (m) => <Link className="underline-offset-2 hover:underline" href={`/machines/${encodeURIComponent(m.id)}`}>{m.alias}</Link> },
        { header: "State", cell: (m) => <span className="flex gap-1"><StatusBadge state={m.state} />{m.deleted_at ? <StatusBadge state="deleted" /> : m.stale ? <StatusBadge state="stale" /> : m.ready ? <StatusBadge state="ready" /> : null}</span> },
        { header: "Platform", cell: (m) => `${m.platform}/${m.architecture}` },
        { header: "Owner", cell: (m) => <Mono>{m.team_id || m.account_id}</Mono> },
        { header: "Last seen", cell: (m) => (m.last_seen_at ? <Time iso={m.last_seen_at} relative /> : "never") },
      ]}
    />
  )
}
