"use client"

import { useMemo, useState } from "react"
import { FilterSelect, SearchBox } from "@/components/filters"
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
    <Page title="Machines" description="Every enrolled machine. Search by machine id, name, or owner account or team id. Removed machines are hidden.">
      <div className="flex flex-wrap items-center gap-2">
        <SearchBox label="Search machines" placeholder="Id, name or owner" value={q} onChange={setQ} />
        <FilterSelect label="State" value={state} onChange={setState} options={[["", "Any state"], ["online", "Online"], ["offline", "Offline"], ["pending", "Pending"], ["disconnected", "Disconnected"], ["revoked", "Revoked"]]} />
        <FilterSelect label="Platform" value={platform} onChange={setPlatform} options={[["", "Any platform"], ["linux", "Linux"], ["darwin", "macOS"], ["windows", "Windows"]]} />
        <FilterSelect label="Freshness" value={stale} onChange={setStale} options={[["", "Any freshness"], ["false", "Seen recently"], ["true", "Stale or never seen"]]} />
      </div>
      <MachinesTable list={list} empty="No machine matches these filters." />
    </Page>
  )
}
