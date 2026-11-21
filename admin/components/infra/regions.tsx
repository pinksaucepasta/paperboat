"use client"

import { useMemo, useState } from "react"
import { ActionDialog } from "@/components/action-dialog"
import { RowMenu, type RowAction } from "@/components/row-menu"
import { DataTable } from "@/components/data-table"
import { StatusBadge } from "@/components/filters"
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
      description="Nodes enrol only into enabled regions."
      target={id || "-"}
      submitLabel="Create region"
      valid={slug.test(id) && name.length >= 1 && name.length <= 80}
      trigger={(open) => <Button size="lg" onClick={open}>Create a region</Button>}
      run={(h) => operatorAction("POST", `/v1/admin/regions/${encodeURIComponent(id)}/create`, h, { revision: 0, display_name: name })}
      onDone={() => { setId(""); setName("") }}
    >
      <TextField label="Region id" value={id} onChange={setId} hint="Lowercase letters, digits, _ and -. Can’t change later." />
      <TextField label="Display name" value={name} onChange={setName} maxLength={80} />
    </ActionDialog>
  )
}

function RegionActions({ region }: { region: Region }) {
  const [name, setName] = useState(region.display_name)
  const path = (a: string) => `/v1/admin/regions/${encodeURIComponent(region.id)}/${a}`
  const actions: RowAction[] = [
    {
      key: "rename",
      label: "Rename",
      dialog: (c) => (
        <ActionDialog
          {...c}
          title={`Rename ${region.id}`}
          description="Changes the display name."
          target={region.id}
          submitLabel="Save name"
          valid={name.length >= 1 && name.length <= 80}
          run={(h) => operatorAction("POST", path("update"), h, { revision: region.revision, display_name: name })}
        >
          <TextField label="Display name" value={name} onChange={setName} maxLength={80} />
        </ActionDialog>
      ),
    },
    region.enabled
      ? {
          key: "disable",
          label: "Disable",
          destructive: true,
          dialog: (c) => (
            <ActionDialog
              {...c}
              title={`Disable ${region.id}`}
              description="New nodes can’t enrol here; its nodes are fenced and drained, and its workspaces move to other regions now. Re-enabling doesn’t resume them."
              target={region.id}
              destructive
              confirmWord={region.id}
              submitLabel="Disable region"
              run={(h) => operatorAction("POST", path("disable"), h, { revision: region.revision })}
            />
          ),
        }
      : {
          key: "enable",
          label: "Enable",
          dialog: (c) => (
            <ActionDialog
              {...c}
              title={`Enable ${region.id}`}
              description="Nodes can enrol here again. Drained nodes stay drained until resumed."
              target={region.id}
              submitLabel="Enable region"
              run={(h) => operatorAction("POST", path("enable"), h, { revision: region.revision })}
            />
          ),
        },
  ]
  if (!region.enabled)
    actions.push({
      key: "replace",
      label: "Re-place workspaces now",
      dialog: (c) => (
        <ActionDialog
          {...c}
          title={`Re-place ${region.id}’s workspaces`}
          description="Moves every workspace whose edge pair uses this region onto other regions now and wakes their machines. A workspace with no other region available keeps its pair."
          target={region.id}
          submitLabel="Re-place workspaces"
          run={(h) => operatorAction("POST", path("replace"), h, { revision: region.revision })}
        />
      ),
    })
  actions.push({
    key: "delete",
    label: "Delete region",
    destructive: true,
    dialog: (c) => (
      <ActionDialog
        {...c}
        title={`Delete ${region.id}`}
        description="Permanently removes an unused region."
        target={region.id}
        destructive
        confirmWord={region.id}
        submitLabel="Delete region"
        run={(h) => operatorAction("DELETE", `/v1/admin/regions/${encodeURIComponent(region.id)}`, h)}
      />
    ),
  })
  return <RowMenu label={region.id} actions={actions} data-region-actions={region.id} />
}

export function RegionsTab() {
  const admin = useOperator()?.role === "admin"
  const filters = useMemo(() => ({}), [])
  const list = useCursorList<Regions>("/v1/admin/regions", filters)
  return (
    <div className="flex flex-col gap-3">
      {admin && <div className="flex justify-end"><CreateRegion /></div>}
      <DataTable
        what="regions"
        list={list}
        rows={(d) => d.items}
        rowKey={(r) => r.id}
        empty="No regions yet."
        columns={[
          { header: "Region", cell: (r) => <span className="font-medium">{r.id}</span> },
          { header: "Name", cell: (r) => r.display_name },
          { header: "State", cell: (r) => <StatusBadge state={r.enabled ? "enabled" : "disabled"} /> },
          { header: "Updated", cell: (r) => <Time iso={r.updated_at} relative /> },
          { header: "", className: "w-12", cell: (r) => (admin ? <RegionActions region={r} /> : null) },
        ]}
      />
    </div>
  )
}
