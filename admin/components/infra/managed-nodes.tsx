"use client"

import { useMemo, useState } from "react"
import { ActionDialog } from "@/components/action-dialog"
import { RowMenu, type RowAction } from "@/components/row-menu"
import { DataTable } from "@/components/data-table"
import { Mono, StatusBadge } from "@/components/filters"
import { TextField } from "@/components/fields"
import { Time } from "@/components/time"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { operatorAction, useOperator, type Data } from "@/lib/api"
import { useCursorList } from "@/lib/paging"

type Nodes = Data<"/v1/admin/nodes">
type Node = Nodes["items"][number]

function NodeActions({ node, admin }: { node: Node; admin: boolean }) {
  const path = (action: string) => `/v1/admin/nodes/${encodeURIComponent(node.id)}/${action}`
  /** `confirm`: the operator types the node id first (destructive actions, and drains that cut live connections). */
  const simple = (action: string, label: string, title: string, description: string, destructive = false, confirm = destructive): RowAction => ({
    key: action,
    label,
    destructive,
    dialog: (c) => (
      <ActionDialog
        {...c}
        title={title}
        description={description}
        target={node.id}
        destructive={destructive}
        confirmWord={confirm ? node.id : undefined}
        submitLabel={label}
        run={(h) => operatorAction("POST", path(action), h, { revision: node.revision })}
      />
    ),
  })
  const [site, setSite] = useState("")
  if (!admin) return null
  const actions: RowAction[] = [
    {
      key: "site",
      label: "Set site",
      dialog: (d) => (
        <ActionDialog
          {...d}
          title={`Set the site of ${node.id}`}
          description="The city or site shown next to the region."
          target={node.id}
          submitLabel="Save site"
          valid={site.length >= 1 && site.length <= 80}
          run={(h) => operatorAction("POST", path("site"), h, { site })}
        >
          <TextField label="Site" value={site} onChange={setSite} maxLength={80} />
        </ActionDialog>
      ),
    },
    ...(node.ready ? [simple("verify-ingress", "Verify ingress", `Verify public ingress of ${node.id}`, "Probes the node’s public address and records the result.")] : []),
    simple("drain", "Drain", `Drain ${node.id}`, "It stops taking new connections; existing ones finish. Other nodes of its region keep serving.", false, true),
    simple("drain-move", "Drain and move", `Drain ${node.id} and move`, "It stops taking new connections and every machine attached to it re-attaches now, to the other nodes of its region or the backup region. Relay clients are moved off within 90 seconds.", false, true),
    simple("resume", "Resume", `Resume ${node.id}`, "It takes work again once it reports fresh observations."),
    simple("retire", "Retire", `Retire ${node.id}`, node.carriers > 0 ? `${node.carriers} machine${node.carriers === 1 ? " is" : "s are"} still attached to it and will lose it now. Drain it and wait for 0 carriers first. Removes the node from the fleet for good.` : "Removes the node from the fleet for good.", true),
  ]
  return <RowMenu label={node.id} actions={actions} data-node-actions={node.id} />
}

/** Mesh links, with the failed forwards (unreachable + holder_miss) beside them, muted while there are none. */
function MeshCell({ forward }: { forward: Node["forward"] }) {
  if (!forward) return "-"
  const failed = (forward.unreachable ?? 0) + (forward.holder_miss ?? 0)
  return (
    <span className="inline-flex items-baseline gap-1.5">
      <Mono>{forward.links ?? 0}</Mono>
      <span className={failed === 0 ? "text-muted-foreground" : "text-destructive"}><Mono>{failed} failed</Mono></span>
    </span>
  )
}

export function ManagedNodes() {
  const admin = useOperator()?.role === "admin"
  const filters = useMemo(() => ({}), [])
  const list = useCursorList<Nodes>("/v1/admin/nodes", filters)
  // The display name users see for a region; the id when the list is not loaded.
  const regionName = new Map((regionList.data?.items ?? []).map((r) => [r.id, r.display_name]))
  return (
    <div className="flex flex-col gap-3">
      <p className="text-sm text-muted-foreground">Paperboat’s nodes are pbh installations claimed for everyone: run <code>pbh install --region &lt;id&gt;</code> on the host and claim it under Self-hosting. Its region is created here on first use.</p>
      <DataTable
        what="nodes"
        list={list}
        rows={(d) => d.items}
        rowKey={(n) => n.id}
        empty="No managed nodes yet."
        columns={[
          { header: "Node", cell: (n) => <span className="font-medium">{n.id}</span> },
          { header: "Region", cell: (n) => regionName.get(n.region_id) ?? n.region_id },
          { header: "Roles", cell: (n) => n.roles.join(", ") },
          { header: "Protocol", cell: (n) => <Mono>{n.protocol_version}</Mono> },
          {
            header: "State",
            cell: (n) => (
              <span className="inline-flex flex-wrap items-center gap-2">
                <StatusBadge state={n.ready ? n.state : "stale"} />
                {n.state === "ready" && n.in_rotation === false && <Badge variant="outline" className="font-normal text-muted-foreground">Paused for load</Badge>}
              </span>
            ),
          },
          { header: "Carriers", cell: (n) => n.carriers },
          { header: "Load", cell: (n) => (n.load_percent === undefined ? <span className="text-muted-foreground">-</span> : `${Math.round(n.load_percent)}%`) },
          { header: "Ingress verified", cell: (n) => (n.ingress_verified ? "Yes" : <span className="text-muted-foreground">No</span>) },
          { header: "Forwarded", cell: (n) => (n.forward ? <Mono>{n.forward.forwarded}</Mono> : "-") },
          { header: "Mesh", cell: (n) => <MeshCell forward={n.forward} /> },
          { header: "Seen", cell: (n) => (n.observed_at ? <Time iso={n.observed_at} relative /> : "Waiting") },
          { header: "", className: "w-12", cell: (n) => <NodeActions node={n} admin={admin} /> },
        ]}
      />
    </div>
  )
}
