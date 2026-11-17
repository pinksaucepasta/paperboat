"use client"

import { useMemo } from "react"
import { MachinesTable } from "@/components/machines-table"
import { useCursorList } from "@/lib/paging"
import type { Data } from "@/lib/api"

/** The machines one account or team owns, as the same paged table the Machines page uses. */
export function MachinesOf({ owner }: { owner: { account: string } | { team: string } }) {
  const filters = useMemo(() => ({ ...owner, deleted: "true" }) as Record<string, string>, [owner])
  const list = useCursorList<Data<"/v1/admin/machines">>("/v1/admin/machines", filters)
  return <MachinesTable list={list} empty="This owner has no machines." />
}
