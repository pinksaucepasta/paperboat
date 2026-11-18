"use client"

import { useMemo, useState } from "react"
import { AnnouncementDialog, AnnouncementState, RetractDialog, audienceLabel } from "@/components/announcements"
import { DataTable } from "@/components/data-table"
import { Filters, Mono } from "@/components/filters"
import { Page } from "@/components/page"
import { RowMenu, type RowAction } from "@/components/row-menu"
import { Time } from "@/components/time"
import { useOperator, type Data } from "@/lib/api"
import { useCursorList } from "@/lib/paging"

type Item = Data<"/v1/admin/announcements">["announcements"][number]

const actionsOf = (item: Item): RowAction[] =>
  item.state === "retracted"
    ? []
    : [
        { key: "edit", label: "Edit", dialog: (control) => <AnnouncementDialog existing={item} control={control} /> },
        { key: "retract", label: "Retract", destructive: true, dialog: (control) => <RetractDialog item={item} control={control} /> },
      ]

export default function AnnouncementsPage() {
  const admin = useOperator()?.role === "admin"
  const [state, setState] = useState("")
  const filters = useMemo(() => ({ state }), [state])
  const list = useCursorList<Data<"/v1/admin/announcements">>("/v1/admin/announcements", filters)
  return (
    <Page title="Announcements">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <Filters items={[{ label: "State", value: state, onChange: setState, options: [["scheduled", "Scheduled"], ["live", "Live"], ["ended", "Ended"], ["retracted", "Retracted"]] }]} />
        {admin && <AnnouncementDialog />}
      </div>
      <DataTable
        what="announcements"
        list={list}
        rows={(d) => d.announcements}
        rowKey={(a) => a.id}
        empty="No announcements."
        columns={[
          { header: "Title", cell: (a) => (
            <span className="flex min-w-0 flex-col">
              <span className="truncate font-medium">{a.title}</span>
              <span className="text-xs text-muted-foreground">{a.kind === "banner" ? `Banner · ${a.severity}` : "Notice"}</span>
            </span>
          ) },
          { header: "Audience", className: "hidden md:table-cell max-w-56", cell: (a) => <span className="block truncate"><Mono>{audienceLabel(a)}</Mono></span> },
          { header: "Time", className: "hidden 2xl:table-cell", cell: (a) => (
            <span className="text-muted-foreground"><Time iso={a.starts_at} />{a.ends_at ? <> – <Time iso={a.ends_at} /></> : " – no end"}</span>
          ) },
          { header: "Seen", className: "hidden md:table-cell", cell: (a) => <span className="tabular-nums">{a.kind === "banner" ? `${a.dismissals} dismissed` : `${a.reads} read`}</span> },
          { header: "State", cell: (a) => <AnnouncementState item={a} /> },
          { header: "", cell: (a) => (admin ? <RowMenu label={a.title} actions={actionsOf(a)} /> : null) },
        ]}
      />
    </Page>
  )
}
