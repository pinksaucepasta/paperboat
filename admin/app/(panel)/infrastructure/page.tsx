"use client"

import { useMemo, useState } from "react"
import { DataTable } from "@/components/data-table"
import { Mono, StatusBadge } from "@/components/filters"
import { Page } from "@/components/page"
import { Tabs } from "@/components/tabs"
import { Time } from "@/components/time"
import type { Data } from "@/lib/api"
import { useCursorList } from "@/lib/paging"

type View = "nodes" | "regions" | "relays" | "tunnels"

export default function InfrastructurePage() {
  const [view, setView] = useState<View>("nodes")
  return (
    <Page title="Relays & edges" description="Runtime nodes, regions, relay health and durable tunnels. A stale observation is never shown as ready.">
      <Tabs
        value={view}
        onChange={setView}
        options={[["nodes", "Nodes"], ["regions", "Regions"], ["relays", "Relay health"], ["tunnels", "Tunnels"]]}
      />
      {view === "nodes" && <Nodes />}
      {view === "regions" && <Regions />}
      {view === "relays" && <Health kind="relay" />}
      {view === "tunnels" && <Health kind="tunnel" />}
    </Page>
  )
}

function Nodes() {
  const filters = useMemo(() => ({}), [])
  const list = useCursorList<Data<"/v1/admin/nodes">>("/v1/admin/nodes", filters)
  return (
    <DataTable
      what="nodes"
      list={list}
      rows={(d) => d.items}
      rowKey={(n) => n.id}
      empty="No runtime node is enrolled."
      columns={[
        { header: "Node", cell: (n) => <Mono>{n.id}</Mono> },
        { header: "Region", cell: (n) => n.region_id },
        { header: "Roles", cell: (n) => n.roles.join(", ") },
        { header: "Protocol", cell: (n) => <Mono>{n.protocol_version}</Mono> },
        { header: "State", cell: (n) => <span className="flex gap-1"><StatusBadge state={n.state} /><StatusBadge state={n.ready ? "ready" : "stale"} /></span> },
        { header: "Endpoint", cell: (n) => `${n.endpoint_host}:${n.endpoint_tcp_port}` },
        { header: "Last heartbeat", cell: (n) => (n.observed_at ? <Time iso={n.observed_at} relative /> : "never") },
      ]}
    />
  )
}

function Regions() {
  const filters = useMemo(() => ({}), [])
  const list = useCursorList<Data<"/v1/admin/regions">>("/v1/admin/regions", filters)
  return (
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
      ]}
    />
  )
}

function Health({ kind }: { kind: "relay" | "tunnel" }) {
  const filters = useMemo(() => ({ kind }), [kind])
  const list = useCursorList<Data<"/v1/admin/infrastructure">>("/v1/admin/infrastructure", filters)
  return (
    <DataTable
      what={kind === "relay" ? "relay health" : "tunnels"}
      list={list}
      rows={(d) => d.items}
      rowKey={(i) => i.id}
      empty={kind === "relay" ? "No relay has registered." : "No durable tunnel exists."}
      columns={[
        { header: kind === "relay" ? "Relay" : "Tunnel", cell: (i) => <Mono>{i.id}</Mono> },
        { header: "Observed", cell: (i) => <StatusBadge state={i.observed_status === "not_ready" ? "failed" : i.observed_status === "unknown" ? "uncertain" : i.observed_status} /> },
        { header: "Desired", cell: (i) => i.desired_state ?? i.state },
        { header: "Last observed", cell: (i) => (i.observed_at ? <Time iso={i.observed_at} relative /> : "never") },
        { header: "Revision", cell: (i) => i.revision, className: "text-right tabular-nums" },
      ]}
    />
  )
}
