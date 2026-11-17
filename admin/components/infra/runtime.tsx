"use client"

import { useMemo, useState } from "react"
import { ActionDialog } from "@/components/action-dialog"
import { DataTable } from "@/components/data-table"
import { FilterSelect, Mono, StatusBadge } from "@/components/filters"
import { TextField } from "@/components/fields"
import { Time } from "@/components/time"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { operatorAction, useCanOperate, type Data } from "@/lib/api"
import { useCursorList } from "@/lib/paging"

type Infrastructure = Data<"/v1/admin/infrastructure">
type Item = Infrastructure["items"][number]

/** Relay health and tunnels, each row with its operator actions (they take the row's current revision). */
export function Health({ kind }: { kind: "relay" | "tunnel" }) {
  const filters = useMemo(() => ({ kind }), [kind])
  const list = useCursorList<Infrastructure>("/v1/admin/infrastructure", filters)
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
        { header: "Actions", cell: (i) => <RowActions item={i} kind={kind} />, className: "min-w-[10rem] whitespace-normal" },
      ]}
    />
  )
}

function RowActions({ item, kind }: { item: Item; kind: "relay" | "tunnel" }) {
  const base = kind === "relay" ? `/v1/admin/relays/${encodeURIComponent(item.id)}` : `/v1/admin/tunnels/${encodeURIComponent(item.id)}`
  const [stop, go] = kind === "relay" ? (["drain", "resume"] as const) : (["disable", "enable"] as const)
  const noun = kind === "relay" ? "relay" : "tunnel"
  const body = { revision: item.revision }
  if (!useCanOperate()) return <span className="text-xs text-muted-foreground">Operations role only</span>
  return (
    <div className="flex flex-wrap gap-1" data-row-actions={item.id}>
      <ActionDialog
        title={`${stop === "drain" ? "Drain" : "Disable"} ${item.id}`}
        description={kind === "relay" ? "The relay stops taking new sessions; held ones finish. Reversible with Resume." : "Pauses the tunnel: it stops serving traffic. Reversible with Enable."}
        target={item.id}
        submitLabel={stop === "drain" ? "Drain relay" : "Disable tunnel"}
        trigger={(open) => <Button size="xs" variant="outline" onClick={open}>{stop === "drain" ? "Drain" : "Disable"}</Button>}
        run={(h) => operatorAction("POST", `${base}/${stop}`, h, body)}
      />
      <ActionDialog
        title={`${go === "resume" ? "Resume" : "Enable"} ${item.id}`}
        description={`Puts the ${noun} back in service.`}
        target={item.id}
        submitLabel={go === "resume" ? "Resume relay" : "Enable tunnel"}
        trigger={(open) => <Button size="xs" variant="outline" onClick={open}>{go === "resume" ? "Resume" : "Enable"}</Button>}
        run={(h) => operatorAction("POST", `${base}/${go}`, h, body)}
      />
    </div>
  )
}

/** Block or unblock a published hostname (abuse, legal takedown). There is no list of publications: name the host. */
export function Publications() {
  const [host, setHost] = useState("")
  const [revision, setRevision] = useState("1")
  const [legal, setLegal] = useState(false)
  const hostOk = /^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$/.test(host)
  const revisionOk = Number.isInteger(+revision) && +revision >= 1
  const can = useCanOperate()
  if (!can) return <p className="text-sm text-muted-foreground">Blocking or unblocking a hostname needs the operations role.</p>
  return (
    <section className="flex flex-col gap-3" data-publications>
      <p className="text-sm text-muted-foreground">Block a published hostname so the edge refuses it, or lift a block. The revision is 1 for a name that was never blocked, otherwise its current block version.</p>
      <div className="grid max-w-xl gap-3">
        <TextField label="Hostname" value={host} onChange={(v) => setHost(v.trim().toLowerCase())} placeholder="demo.pub.example.test" />
        <TextField label="Revision" inputMode="numeric" value={revision} onChange={setRevision} />
        <label className="flex items-center gap-2 text-sm"><Checkbox checked={legal} onCheckedChange={(v) => setLegal(v === true)} />Legal takedown (answered 451, the owner is notified)</label>
      </div>
      <div className="flex gap-2">
        <ActionDialog
          title={`Block ${host}`}
          description={legal ? "The edge answers 451 for this hostname and the owner receives a legal notice. Reversible with Unblock." : "The edge refuses this hostname (403). Reversible with Unblock."}
          target={host}
          destructive
          confirmWord={host}
          submitLabel="Block hostname"
          valid={hostOk && revisionOk}
          trigger={(open) => <Button variant="destructive" disabled={!hostOk || !revisionOk} onClick={open}>Block</Button>}
          run={(h) => operatorAction("POST", `/v1/admin/publications/${encodeURIComponent(host)}/block`, h, { revision: +revision, ...(legal ? { legal: true } : {}) })}
        />
        <ActionDialog
          title={`Unblock ${host}`}
          description="Lets the edge serve this hostname again. The block and its evidence stay in the history."
          target={host}
          submitLabel="Unblock hostname"
          valid={hostOk && revisionOk}
          trigger={(open) => <Button variant="outline" disabled={!hostOk || !revisionOk} onClick={open}>Unblock</Button>}
          run={(h) => operatorAction("POST", `/v1/admin/publications/${encodeURIComponent(host)}/unblock`, h, { revision: +revision })}
        />
      </div>
    </section>
  )
}

type Operations = Data<"/v1/admin/control-operations">

/** Control operations; a dead-lettered one can be put back in the queue. */
export function ControlOperations() {
  const can = useCanOperate()
  const [state, setState] = useState("")
  const filters = useMemo(() => ({ state }), [state])
  const list = useCursorList<Operations>("/v1/admin/control-operations", filters)
  return (
    <div className="flex flex-col gap-3">
      <FilterSelect label="Operation state" value={state} onChange={setState} options={[["", "Any state"], ["pending", "Pending"], ["running", "Running"], ["succeeded", "Succeeded"], ["failed", "Failed"], ["uncertain", "Uncertain"], ["dead_letter", "Dead-lettered"]]} />
      <DataTable
        what="control operations"
        list={list}
        rows={(d) => d.operations}
        rowKey={(o) => o.id}
        empty="No control operation matches this state."
        columns={[
          { header: "Operation", cell: (o) => <Mono>{o.id}</Mono> },
          { header: "Type", cell: (o) => o.type },
          { header: "State", cell: (o) => <StatusBadge state={o.state} /> },
          { header: "Attempts", cell: (o) => o.attempts, className: "text-right tabular-nums" },
          { header: "Updated", cell: (o) => <Time iso={o.updated_at} relative /> },
          {
            header: "Actions",
            cell: (o) =>
              o.state === "dead_letter" && can ? (
                <ActionDialog
                  title={`Recover ${o.id}`}
                  description="Puts this dead-lettered operation back in the queue to run again."
                  target={o.id}
                  submitLabel="Recover operation"
                  trigger={(open) => <Button size="xs" variant="outline" onClick={open}>Recover</Button>}
                  run={(h) => operatorAction("POST", `/v1/admin/control-operations/${encodeURIComponent(o.id)}/recover`, h)}
                />
              ) : (
                "—"
              ),
          },
        ]}
      />
    </div>
  )
}
