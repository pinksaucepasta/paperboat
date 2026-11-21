"use client"

import { useMemo, useState } from "react"
import { ActionDialog } from "@/components/action-dialog"
import { CommandBlock } from "@/components/command-block"
import { Panel } from "@/components/page"
import { RowMenu, type RowAction } from "@/components/row-menu"
import { ErrorBlock } from "@/components/error-block"
import { Mono, StatusBadge } from "@/components/filters"
import { TextField } from "@/components/fields"
import { Time } from "@/components/time"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { PagerBar } from "@/components/data-table"
import { useCursorList } from "@/lib/paging"
import { operatorAction, useOperator, type Data } from "@/lib/api"

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
    <Panel title="Enrol a server node">
      <div className="flex flex-col gap-4" data-enrol-server-node>
        <div className="grid gap-3 md:grid-cols-3">
          <TextField label="Region" value={region} onChange={setRegion} placeholder="hel" />
          <TextField label="Instance" value={instance} onChange={setInstance} placeholder="slot-01" />
          <TextField label="Hostname" value={host} onChange={(v) => setHost(v.trim())} placeholder="server-3.example.test" />
        </div>
        <div className="flex flex-col gap-1.5">
          <span className="text-sm font-medium">Run on the host</span>
          {command ? <CommandBlock command={command} data-install-command /> : <p className="rounded-lg bg-muted px-3.5 py-3 text-xs text-muted-foreground">Fill in region, instance and hostname.</p>}
          <span className="text-xs text-muted-foreground">It prints a claim code (15 minutes) and waits on port 7277. Afterwards start it with:</span>
          <CommandBlock command="paperboat-server serve --config /etc/paperboat/node.json" />
        </div>
        <ActionDialog
          title="Enrol the server node"
          description="An enrolled node claims it on port 7277 and sends its configuration."
          target={address || "-"}
          submitLabel="Enrol server node"
          valid={address.length > 0 && address.length <= 300 && code.length > 0 && code.length <= 200}
          trigger={(open) => <div><Button size="lg" disabled={!ready} onClick={() => { if (!address) setAddress(`${host}:7277`); open() }}>I ran it, enrol the node</Button></div>}
          run={(h) => operatorAction("POST", "/v1/admin/server-nodes/enrollments", h, { address, code })}
          onDone={() => { setCode(""); setAddress("") }}
        >
          <TextField label="Claim address" value={address} onChange={(v) => setAddress(v.trim())} placeholder={host ? `${host}:7277` : "host:7277"} />
          <TextField label="Claim code" value={code} onChange={(v) => setCode(v.trim())} type="password" autoComplete="off" hint="pbs1.… Used once." />
        </ActionDialog>
      </div>
    </Panel>
  )
}

function Actions({ n }: { n: ServerNode }) {
  const path = (a: string) => `/v1/admin/server-nodes/${encodeURIComponent(n.id)}/${a}`
  const actions: RowAction[] = [
    n.state === "draining"
      ? {
          key: "resume",
          label: "Resume",
          dialog: (c) => (
            <ActionDialog
              {...c}
              title={`Resume ${n.id}`}
              description="Cancels the drain. Works only in the first seconds; after that, remove it and enrol a new node."
              target={n.id}
              submitLabel="Resume node"
              run={(h) => operatorAction("POST", path("resume"), h, {})}
            />
          ),
        }
      : {
          key: "drain",
          label: "Drain",
          destructive: true,
          dialog: (c) => (
            <ActionDialog
              {...c}
              title={`Drain ${n.id}`}
              description="Takes it out of the region map and DNS, then shuts it down once connections finish."
              target={n.id}
              destructive
              confirmWord={n.id}
              submitLabel="Drain node"
              run={(h) => operatorAction("POST", path("drain"), h, {})}
            />
          ),
        },
    {
      key: "remove",
      label: "Remove",
      destructive: true,
      dialog: (c) => (
        <ActionDialog
          {...c}
          title={`Remove ${n.id}`}
          description="Its heartbeats are refused and it can’t rejoin. Enrol a new node to replace it."
          target={n.id}
          destructive
          confirmWord={n.id}
          submitLabel="Remove node"
          run={(h) => operatorAction("POST", path("remove"), h, {})}
        />
      ),
    },
  ]
  return <RowMenu label={n.id} actions={actions} data-server-node-actions={n.id} />
}

export function ServerNodesTab() {
  const admin = useOperator()?.role === "admin"
  const filters = useMemo(() => ({}), [])
  const q = useCursorList<Nodes>("/v1/admin/server-nodes", filters, "after", 5000)
  return (
    <div className="flex flex-col gap-8">
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
                {["Node", "Region", "State", "Version", "Seen", ""].map((h) => <th key={h} scope="col" className="h-9 px-3 font-normal">{h}</th>)}
              </tr>
            </thead>
            <tbody className="divide-y divide-divider">
              {q.data.nodes.map((n) => (
                <tr key={n.id} className="[&>td]:h-14 [&>td]:px-3" data-server-node={n.id}>
                  <td><div className="font-medium">{n.id}</div><div className="text-xs text-muted-foreground">{n.public_address}</div></td>
                  <td>{n.region}</td>
                  <td><StatusBadge state={n.state === "active" ? (n.healthy ? "ready" : "unhealthy") : n.state} /></td>
                  <td><Mono>{n.version}</Mono></td>
                  <td>{n.last_seen_at ? <Time iso={n.last_seen_at} relative /> : "Waiting"}</td>
                  <td className="w-12">{admin && <Actions n={n} />}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {q.data.nodes.length === 0 && <p className="py-12 text-center text-sm text-muted-foreground">No server nodes yet.</p>}
          <PagerBar page={q.page} count={q.data.nodes.length} total={q.data.total} hasNext={q.hasNext} go={q.go} />
        </div>
      )}
    </div>
  )
}
