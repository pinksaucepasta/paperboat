"use client"

import { useState } from "react"
import { ActionDialog } from "@/components/action-dialog"
import { ErrorBlock } from "@/components/error-block"
import { Mono, StatusBadge } from "@/components/filters"
import { TextField } from "@/components/fields"
import { Time } from "@/components/time"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { operatorAction, useApiQuery, useOperator, type Data } from "@/lib/api"

type Nodes = Data<"/v1/admin/server-nodes">
type ServerNode = Nodes["nodes"][number]

/** The command the new server host runs: it prints a claim code (15 minutes) and waits to be enrolled. */
export function enrollNodeCommand(region: string, instance: string, host: string): string {
  return `paperboat-server enroll-node -region ${region} -instance ${instance} -host ${host} -config-out /etc/paperboat/node.json`
}

function Enrol() {
  const [region, setRegion] = useState("")
  const [instance, setInstance] = useState("slot-01")
  const [host, setHost] = useState("")
  const [address, setAddress] = useState("")
  const [code, setCode] = useState("")
  const ready = /^[a-z][a-z0-9-]{0,62}$/.test(region) && /^slot-\d{2}$/.test(instance) && host.length > 0
  const command = ready ? enrollNodeCommand(region, instance, host) : ""
  return (
    <section className="surface flex flex-col gap-4 rounded-lg border p-4" data-enrol-server-node>
      <div>
        <h2 className="text-sm font-medium">Enrol a server node</h2>
        <p className="text-sm text-muted-foreground">A control-plane server node makes its own claim code (15 minutes) and waits on port 8443 for an enrolled node to claim it.</p>
      </div>
      <div className="grid gap-3 md:grid-cols-3">
        <TextField label="Region" value={region} onChange={setRegion} placeholder="hel" />
        <TextField label="Instance" value={instance} onChange={setInstance} hint="slot-NN" />
        <TextField label="Hostname" value={host} onChange={(v) => setHost(v.trim())} placeholder="server-3.example.test" />
      </div>
      <div className="flex flex-col gap-1.5">
        <span className="text-xs font-medium">1. Run on the new server host</span>
        <code className="break-all rounded-md border p-2 font-mono text-xs" data-install-command>{command || "Fill in region, instance and hostname to see the command."}</code>
        <div><Button variant="outline" size="sm" disabled={!command} onClick={() => navigator.clipboard?.writeText(command).catch(() => {})}>Copy command</Button></div>
        <span className="text-xs text-muted-foreground">It prints “Claim code (valid 15m): pbs1.…” and blocks until claimed. Afterwards start it with `paperboat-server serve --config` on the file it wrote.</span>
      </div>
      <ActionDialog
        title="Enrol the server node"
        description="An enrolled node dials the new one, checks it against the code, and sends it its sealed configuration. Reaching port 8443 on the address is required."
        target={address || "-"}
        submitLabel="Enrol server node"
        valid={address.length > 0 && address.length <= 300 && code.length > 0 && code.length <= 200}
        trigger={(open) => <div><Button disabled={!ready} onClick={() => { if (!address) setAddress(`${host}:8443`); open() }}>I ran it, enrol the node</Button></div>}
        run={(h) => operatorAction("POST", "/v1/admin/server-nodes/enrollments", h, { address, code })}
        onDone={() => { setCode(""); setAddress("") }}
      >
        <TextField label="Claim address" value={address} onChange={(v) => setAddress(v.trim())} placeholder={host ? `${host}:8443` : "host:8443"} />
        <TextField label="Claim code" value={code} onChange={(v) => setCode(v.trim())} type="password" autoComplete="off" hint="Printed by enroll-node. Used once." />
      </ActionDialog>
      <p className="text-xs text-muted-foreground">The node appears below as registered, then active and healthy once it serves and heartbeats.</p>
    </section>
  )
}

function Actions({ n }: { n: ServerNode }) {
  const path = (a: string) => `/v1/admin/server-nodes/${encodeURIComponent(n.id)}/${a}`
  return (
    <div className="flex flex-wrap gap-1" data-server-node-actions={n.id}>
      <ActionDialog
        title={`Drain ${n.id}`}
        description="Takes the node out of the region map and DNS and shuts it down gracefully; held connections finish first. There is no resume: to bring capacity back, enrol a new node."
        target={n.id}
        destructive
        confirmWord={n.id}
        submitLabel="Drain node"
        trigger={(open) => <Button size="xs" variant="destructive" onClick={open}>Drain</Button>}
        run={(h) => operatorAction("POST", path("drain"), h, {})}
      />
      <ActionDialog
        title={`Remove ${n.id}`}
        description="Removes the node for good: its heartbeats are refused and it cannot rejoin. Re-enrol a new node to replace it."
        target={n.id}
        destructive
        confirmWord={n.id}
        submitLabel="Remove node"
        trigger={(open) => <Button size="xs" variant="destructive" onClick={open}>Remove</Button>}
        run={(h) => operatorAction("POST", path("remove"), h, {})}
      />
    </div>
  )
}

export function ServerNodesTab() {
  const admin = useOperator()?.role === "admin"
  const q = useApiQuery<Nodes>(["server-nodes"], "/v1/admin/server-nodes", 5000)
  return (
    <div className="flex flex-col gap-4">
      {admin && <Enrol />}
      {q.isError ? (
        <ErrorBlock what="server nodes" error={q.error} retry={() => q.refetch()} />
      ) : !q.data ? (
        <Skeleton className="h-32 w-full" />
      ) : (
        <div data-table="server nodes" data-state={q.data.nodes.length === 0 ? "empty" : "ready"} className="overflow-x-auto">
          <table className="w-full min-w-[40rem] border-collapse text-sm">
            <thead>
              <tr className="border-b border-divider text-left text-xs text-muted-foreground">
                {["Node", "Region", "State", "Version", "Last seen", "Actions"].map((h) => <th key={h} scope="col" className="h-9 px-3 font-medium">{h}</th>)}
              </tr>
            </thead>
            <tbody>
              {q.data.nodes.map((n) => (
                <tr key={n.id} className="border-b border-divider align-top [&>td]:px-3 [&>td]:py-2" data-server-node={n.id}>
                  <td><Mono>{n.id}</Mono><div className="text-xs text-muted-foreground">{n.public_address}</div></td>
                  <td>{n.region}</td>
                  <td className="whitespace-nowrap"><StatusBadge state={n.state === "registered" ? "uncertain" : n.state === "active" ? "active" : "stale"} /> {n.state === "active" && <StatusBadge state={n.healthy ? "ready" : "stale"} />}</td>
                  <td><Mono>{n.version}</Mono></td>
                  <td>{n.last_seen_at ? <Time iso={n.last_seen_at} relative /> : "waiting"}</td>
                  <td>{admin ? <Actions n={n} /> : <span className="text-xs text-muted-foreground">Admin only</span>}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {q.data.nodes.length === 0 && <p className="py-10 text-center text-sm text-muted-foreground">No server node is enrolled.</p>}
        </div>
      )}
    </div>
  )
}
