"use client"

import { useMemo, useState } from "react"
import { ActionDialog } from "@/components/action-dialog"
import { RowMenu, type RowAction } from "@/components/row-menu"
import { DataTable } from "@/components/data-table"
import { Filters, Mono, StatusBadge } from "@/components/filters"
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
      empty={kind === "relay" ? "No relays yet." : "No tunnels yet."}
      columns={[
        { header: kind === "relay" ? "Relay" : "Tunnel", cell: (i) => <span className="font-medium">{i.id}</span> },
        { header: "Observed", cell: (i) => <StatusBadge state={i.observed_status === "not_ready" ? "failed" : i.observed_status} /> },
        { header: "Desired", cell: (i) => <span className="inline-block first-letter:uppercase">{i.desired_state ?? i.state}</span> },
        { header: "Seen", cell: (i) => (i.observed_at ? <Time iso={i.observed_at} relative /> : "Never") },
        { header: "Revision", cell: (i) => i.revision, className: "text-right tabular-nums" },
        { header: "", className: "w-12", cell: (i) => <RowActions item={i} kind={kind} /> },
      ]}
    />
  )
}

function RowActions({ item, kind }: { item: Item; kind: "relay" | "tunnel" }) {
  const base = kind === "relay" ? `/v1/admin/relays/${encodeURIComponent(item.id)}` : `/v1/admin/tunnels/${encodeURIComponent(item.id)}`
  const [stop, go] = kind === "relay" ? (["drain", "resume"] as const) : (["disable", "enable"] as const)
  const body = { revision: item.revision }
  const stopLabel = stop === "drain" ? "Drain" : "Disable"
  const goLabel = go === "resume" ? "Resume" : "Enable"
  if (!useCanOperate()) return null
  const actions: RowAction[] = [
    {
      key: go,
      label: goLabel,
      dialog: (c) => (
        <ActionDialog {...c} title={`${goLabel} ${item.id}`} description="Puts it back in service." target={item.id} submitLabel={go === "resume" ? "Resume relay" : "Enable tunnel"} run={(h) => operatorAction("POST", `${base}/${go}`, h, body)} />
      ),
    },
    {
      key: stop,
      label: stopLabel,
      destructive: true,
      dialog: (c) => (
        <ActionDialog
          {...c}
          title={`${stopLabel} ${item.id}`}
          description={kind === "relay" ? "It stops taking new sessions; held ones finish." : "It stops serving traffic."}
          target={item.id}
          submitLabel={stop === "drain" ? "Drain relay" : "Disable tunnel"}
          run={(h) => operatorAction("POST", `${base}/${stop}`, h, body)}
        />
      ),
    },
  ]
  return <RowMenu label={item.id} actions={actions} data-row-actions={item.id} />
}

type Blocks = Data<"/v1/admin/publications">
type Block = Blocks["items"][number]
const hostPattern = /^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$/

/** Block a hostname never blocked before: its revision is 1. */
function BlockHostname() {
  const [host, setHost] = useState("")
  const [legal, setLegal] = useState(false)
  return (
    <ActionDialog
      title="Block a hostname"
      description="The edge refuses it until it is unblocked."
      target={host || "-"}
      destructive
      confirmWord={host || undefined}
      submitLabel="Block hostname"
      valid={hostPattern.test(host)}
      trigger={(open) => <Button size="lg" onClick={open}>Block hostname</Button>}
      run={(h) => operatorAction("POST", `/v1/admin/publications/${encodeURIComponent(host)}/block`, h, { revision: 1, ...(legal ? { legal: true } : {}) })}
      onDone={() => { setHost(""); setLegal(false) }}
    >
      <TextField label="Hostname" value={host} onChange={(v) => setHost(v.trim().toLowerCase())} placeholder="demo.pub.example.test" />
      <label className="flex items-center gap-2 text-sm"><Checkbox checked={legal} onCheckedChange={(v) => setLegal(v === true)} />Legal takedown (the owner is notified)</label>
    </ActionDialog>
  )
}

function BlockActions({ b }: { b: Block }) {
  const [legal, setLegal] = useState(b.legal)
  const path = (a: string) => `/v1/admin/publications/${encodeURIComponent(b.hostname)}/${a}`
  const actions: RowAction[] = b.blocked
    ? [{
        key: "unblock",
        label: "Unblock",
        dialog: (c) => <ActionDialog {...c} title={`Unblock ${b.hostname}`} description="The edge serves it again. The block stays in the history." target={b.hostname} submitLabel="Unblock hostname" run={(h) => operatorAction("POST", path("unblock"), h, { revision: b.version })} />,
      }]
    : [{
        key: "block",
        label: "Block again",
        destructive: true,
        dialog: (c) => (
          <ActionDialog {...c} title={`Block ${b.hostname}`} description="The edge refuses it until it is unblocked." target={b.hostname} destructive confirmWord={b.hostname} submitLabel="Block hostname" run={(h) => operatorAction("POST", path("block"), h, { revision: b.version, ...(legal ? { legal: true } : {}) })}>
            <label className="flex items-center gap-2 text-sm"><Checkbox checked={legal} onCheckedChange={(v) => setLegal(v === true)} />Legal takedown (the owner is notified)</label>
          </ActionDialog>
        ),
      }]
  return <RowMenu label={b.hostname} actions={actions} data-block-actions={b.hostname} />
}

/** Blocked hostnames (abuse, legal takedowns), and the ones unblocked since; each row offers the opposite action. */
export function Publications() {
  const can = useCanOperate()
  const [blocked, setBlocked] = useState("")
  const filters = useMemo(() => ({ blocked }), [blocked])
  const list = useCursorList<Blocks>("/v1/admin/publications", filters)
  if (!can) return <p className="py-12 text-center text-sm text-muted-foreground">Operations role only.</p>
  return (
    <div className="flex flex-col gap-3" data-publications>
      <div className="flex flex-wrap items-center gap-2">
        <Filters items={[{ label: "State", value: blocked, onChange: setBlocked, options: [["true", "Blocked"], ["false", "Unblocked"]] }]} />
        <div className="ml-auto"><BlockHostname /></div>
      </div>
      <DataTable
        what="hostname blocks"
        list={list}
        rows={(d) => d.items}
        rowKey={(b) => b.hostname}
        empty={blocked ? "No hostnames match." : "No hostname has been blocked."}
        columns={[
          { header: "Hostname", cell: (b) => <span className="font-medium">{b.hostname}</span> },
          { header: "State", cell: (b) => <StatusBadge state={b.blocked ? (b.legal ? "legal_block" : "blocked") : "unblocked"} /> },
          { header: "Reason", className: "hidden md:table-cell", cell: (b) => <span className="text-muted-foreground">{b.reason}</span> },
          { header: "Updated", className: "hidden md:table-cell", cell: (b) => <Time iso={b.updated_at} relative /> },
          { header: "", className: "w-12", cell: (b) => <BlockActions b={b} /> },
        ]}
      />
    </div>
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
      <div className="flex flex-wrap items-center gap-2"><Filters items={[{ label: "Operation state", value: state, onChange: setState, options: [["pending", "Pending"], ["running", "Running"], ["succeeded", "Succeeded"], ["failed", "Failed"], ["uncertain", "Uncertain"], ["dead_letter", "Dead-lettered"]] }]} /></div>
      <DataTable
        what="control operations"
        list={list}
        rows={(d) => d.operations}
        rowKey={(o) => o.id}
        empty="No operations match."
        columns={[
          { header: "Operation", cell: (o) => <Mono>{o.id}</Mono> },
          { header: "Type", cell: (o) => o.type },
          { header: "State", cell: (o) => <StatusBadge state={o.state} /> },
          { header: "Attempts", cell: (o) => o.attempts, className: "text-right tabular-nums" },
          { header: "Updated", cell: (o) => <Time iso={o.updated_at} relative /> },
          {
            header: "",
            className: "text-right",
            cell: (o) =>
              o.state === "dead_letter" && can ? (
                <ActionDialog
                  title={`Recover ${o.id}`}
                  description="Puts it back in the queue to run again."
                  target={o.id}
                  submitLabel="Recover operation"
                  trigger={(open) => <Button variant="outline" onClick={open}>Recover</Button>}
                  run={(h) => operatorAction("POST", `/v1/admin/control-operations/${encodeURIComponent(o.id)}/recover`, h)}
                />
              ) : null,
          },
        ]}
      />
    </div>
  )
}
