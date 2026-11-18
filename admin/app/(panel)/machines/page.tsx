"use client"

import { useMemo, useState } from "react"
import { Filters, SearchBox } from "@/components/filters"
import { MachinesTable } from "@/components/machines-table"
import { Page } from "@/components/page"
import type { Data } from "@/lib/api"
import { useCursorList } from "@/lib/paging"

export default function MachinesPage() {
  const [q, setQ] = useState("")
  const [state, setState] = useState("")
  const [platform, setPlatform] = useState("")
  const [stale, setStale] = useState("")
  const filters = useMemo(() => ({ q, state, platform, stale }), [q, state, platform, stale])
  const list = useCursorList<Data<"/v1/admin/machines">>("/v1/admin/machines", filters)
  return (
    <Page title="Machines">
      <div className="flex flex-wrap items-center gap-2">
        <SearchBox label="Search machines" placeholder="Id, name or owner" value={q} onChange={setQ} />
        <Filters items={[{ label: "State", value: state, onChange: setState, options: [["online", "Online"], ["offline", "Offline"], ["pending", "Pending"], ["disconnected", "Disconnected"], ["revoked", "Revoked"]] }, { label: "Platform", value: platform, onChange: setPlatform, options: [["linux", "Linux"], ["darwin", "macOS"], ["windows", "Windows"]] }, { label: "Freshness", value: stale, onChange: setStale, options: [["false", "Recent"], ["true", "Stale"]] }]} />
      </div>
      <MachinesTable list={list} empty="No machines match." />
    </Page>
  )
}
