"use client"

import { useMemo, useState } from "react"
import { ActionDialog } from "@/components/action-dialog"
import { DataTable } from "@/components/data-table"
import { Mono, StatusBadge } from "@/components/filters"
import { TextField } from "@/components/fields"
import { Time } from "@/components/time"
import { Button } from "@/components/ui/button"
import { operatorAction, useOperator, type Data } from "@/lib/api"
import { useCursorList } from "@/lib/paging"

type Regions = Data<"/v1/admin/regions">
type Region = Regions["items"][number]
const slug = /^[a-z][a-z0-9_-]{0,62}$/

function CreateRegion() {
  const [id, setId] = useState("")
  const [name, setName] = useState("")
  return (
    <ActionDialog
      title="Create a region"
      description="A region groups nodes for placement. Nodes can only be enrolled into an enabled region."
      target={id || "-"}
      submitLabel="Create region"
      valid={slug.test(id) && name.length >= 1 && name.length <= 80}
      trigger={(open) => <Button onClick={open}>Create a region</Button>}
      run={(h) => operatorAction("POST", `/v1/admin/regions/${encodeURIComponent(id)}/create`, h, { revision: 0, display_name: name })}
      onDone={() => { setId(""); setName("") }}
    >
      <TextField label="Region id" value={id} onChange={setId} hint="Lowercase letters, digits, _ and -; starts with a letter. Cannot change." />
      <TextField label="Display name" value={name} onChange={setName} maxLength={80} />
    </ActionDialog>
  )
}

function RegionActions({ region }: { region: Region }) {
  const [name, setName] = useState(region.display_name)
  const path = (a: string) => `/v1/admin/regions/${encodeURIComponent(region.id)}/${a}`
  return (
    <div className="flex flex-wrap gap-1" data-region-actions={region.id}>
      <ActionDialog
        title={`Rename ${region.id}`}
        description="Changes the display name only."
        target={region.id}
        submitLabel="Save name"
        valid={name.length >= 1 && name.length <= 80}
        trigger={(open) => <Button size="xs" variant="outline" onClick={open}>Rename</Button>}
        run={(h) => operatorAction("POST", path("update"), h, { revision: region.revision, display_name: name })}
      >
        <TextField label="Display name" value={name} onChange={setName} maxLength={80} />
      </ActionDialog>
      {region.enabled ? (
        <ActionDialog
          title={`Disable ${region.id}`}
          description="New nodes can no longer enrol here, and its managed nodes are fenced and drained. Re-enabling does not resume them: resume each node explicitly."
          target={region.id}
          destructive
          confirmWord={region.id}
          submitLabel="Disable region"
          trigger={(open) => <Button size="xs" variant="destructive" onClick={open}>Disable</Button>}
          run={(h) => operatorAction("POST", path("disable"), h, { revision: region.revision })}
        />
      ) : (
        <ActionDialog
          title={`Enable ${region.id}`}
          description="Allows enrolment into the region again. Its drained nodes stay drained until each is resumed."
          target={region.id}
          submitLabel="Enable region"
          trigger={(open) => <Button size="xs" variant="outline" onClick={open}>Enable</Button>}
          run={(h) => operatorAction("POST", path("enable"), h, { revision: region.revision })}
        />
      )}
    </div>
  )
}

export function RegionsTab() {
  const admin = useOperator()?.role === "admin"
  const filters = useMemo(() => ({}), [])
  const list = useCursorList<Regions>("/v1/admin/regions", filters)
  return (
    <div className="flex flex-col gap-3">
      {admin && <div><CreateRegion /></div>}
      <DataTable
        what="regions"
        list={list}
        rows={(d) => d.items}
        rowKey={(r) => r.id}
        empty="No region is defined."
        columns={[
          { header: "Region", cell: (r) => <Mono>{r.id}</Mono> },
          { header: "Name", cell: (r) => r.display_name },
          { header: "Enabled", cell: (r) => <StatusBadge state={r.enabled ? "enabled" : "revoked"} /> },
          { header: "Updated", cell: (r) => <Time iso={r.updated_at} relative /> },
          { header: "Actions", cell: (r) => (admin ? <RegionActions region={r} /> : <span className="text-xs text-muted-foreground">Admin only</span>), className: "min-w-[12rem] whitespace-normal" },
        ]}
      />
    </div>
  )
}
