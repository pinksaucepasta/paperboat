"use client"

import { useState } from "react"
import { ActionDialog } from "@/components/action-dialog"
import { ErrorBlock } from "@/components/error-block"
import { Mono, StatusBadge } from "@/components/filters"
import { SelectField, TextField } from "@/components/fields"
import { Time } from "@/components/time"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { operatorAction, useApiQuery, useOperator, type Data } from "@/lib/api"

type Inventory = Data<"/v1/admin/selfhost/installations">
type Installation = Inventory["installations"][number]

const kinds: [string, string][] = [["relay", "Relay"], ["tunnel", "Edge (previews and tunnels)"], ["both", "Relay and edge"]]
const slug = /^[a-z][a-z0-9_-]{0,62}$/
const host = /^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$/

/** The command the operator runs on the new host; the one-time code is made there, by `pbh`, not by the server. */
export function installCommand(kind: string, endpointHost: string, region: string, name: string): string {
  const parts = ["sudo pbh install", `--enable ${kind}`, `--endpoint-host ${endpointHost}`, `--region ${region}`]
  if (name) parts.push(`--name '${name.replaceAll("'", "")}'`)
  return parts.join(" ")
}

/**
 * Enrol a relay/edge: the node makes its own one-time code (15 minutes), so the steps are: run the command on the
 * host, paste the claim address and code it prints here, then watch it go from waiting to ready.
 */
function EnrolFlow() {
  const [kind, setKind] = useState("relay")
  const [endpoint, setEndpoint] = useState("")
  const [region, setRegion] = useState("selfhost")
  const [name, setName] = useState("")
  const [address, setAddress] = useState("")
  const [code, setCode] = useState("")
  const ready = host.test(endpoint) && slug.test(region) && name.length <= 80
  const command = ready ? installCommand(kind, endpoint, region, name) : ""
  return (
    <section className="surface flex flex-col gap-4 rounded-lg border p-4" data-enrol-node>
      <div>
        <h2 className="text-sm font-medium">Enrol a node</h2>
        <p className="text-sm text-muted-foreground">Install on the new host, then paste what it prints. The one-time code is created on the host and lasts 15 minutes; it is never stored by the server.</p>
      </div>
      <div className="grid gap-3 md:grid-cols-2">
        <SelectField label="What it runs" value={kind} onChange={setKind} options={kinds} />
        <TextField label="Public hostname or IP" value={endpoint} onChange={(v) => setEndpoint(v.trim())} placeholder="relay-1.example.test" />
        <TextField label="Region" value={region} onChange={setRegion} hint="Where it is placed (a label such as hel or sin)." />
        <TextField label="Location / provider label" value={name} onChange={setName} maxLength={80} hint="Shown in the fleet list, e.g. Hetzner Helsinki." />
      </div>
      <div className="flex flex-col gap-1.5">
        <span className="text-xs font-medium">1. Run on the new host (needs root, Docker and systemd)</span>
        <code className="break-all rounded-md border p-2 font-mono text-xs" data-install-command>{command || "Fill in the hostname and region to see the command."}</code>
        <div className="flex gap-2">
          <Button variant="outline" size="sm" disabled={!command} onClick={() => navigator.clipboard?.writeText(command).catch(() => {})}>Copy command</Button>
        </div>
        <span className="text-xs text-muted-foreground">First time on this host? Install pbh first with the installer from your release page, then run the command. It prints a claim address and a code (pbh1.…).</span>
      </div>
      <div className="flex flex-col gap-1.5">
        <span className="text-xs font-medium">2. Paste the claim address and code it printed</span>
        <ActionDialog
          title="Enrol the node"
          description="The server connects to the claim address, verifies the node against the code, registers it as global fleet infrastructure and hands it its control-plane configuration."
          target={address || "-"}
          submitLabel="Enrol node"
          valid={address.length > 0 && /^pbh1\.[A-Za-z0-9_-]{43}\.[A-Za-z0-9_-]{43}$/.test(code)}
          trigger={(open) => <div><Button disabled={!ready} onClick={() => { if (!address && endpoint) setAddress(`${endpoint}:8443`); open() }}>I ran it, enrol the node</Button></div>}
          run={(h) => operatorAction("POST", "/v1/admin/selfhost/enrollments", h, { address, code })}
          onDone={() => { setCode(""); setAddress("") }}
        >
          <TextField label="Claim address" value={address} onChange={(v) => setAddress(v.trim())} placeholder={endpoint ? `${endpoint}:8443` : "host:8443"} hint="Printed as “Claim address”." />
          <TextField label="Self-host code" value={code} onChange={(v) => setCode(v.trim())} type="password" autoComplete="off" hint="Printed as “Self-host code”. Used once; never shown again." />
        </ActionDialog>
      </div>
      <p className="text-xs text-muted-foreground">3. The node appears below as pending, then ready once it heartbeats (an edge also needs its public HTTPS verified). The list refreshes every few seconds.</p>
    </section>
  )
}

function Row({ i, admin }: { i: Installation; admin: boolean }) {
  return (
    <tr className="border-b border-divider align-top [&>td]:px-3 [&>td]:py-2" data-installation={i.installation_id}>
      <td><Mono>{i.name || i.node_id}</Mono><div className="text-xs text-muted-foreground">{i.endpoint_host || "address pending"}</div></td>
      <td>{i.capability}</td>
      <td className="whitespace-nowrap"><StatusBadge state={i.enrollment_state === "pending" ? "uncertain" : "active"} /> <StatusBadge state={i.ready ? "ready" : "stale"} /></td>
      <td>{i.scope_kind}</td>
      <td>{i.last_heartbeat_at ? <Time iso={i.last_heartbeat_at} relative /> : "waiting"}</td>
      <td>
        {admin ? (
          <ActionDialog
            title={`Remove ${i.name || i.node_id}`}
            description="Retires this installation: it stops serving, its credentials and usage keys are revoked, and it cannot be re-enrolled with the same code. The host keeps running until it is uninstalled there."
            target={i.installation_id}
            destructive
            confirmWord={i.installation_id}
            submitLabel="Remove installation"
            trigger={(open) => <Button size="xs" variant="destructive" onClick={open}>Remove</Button>}
            run={(h) => operatorAction("DELETE", `/v1/admin/selfhost/installations/${encodeURIComponent(i.installation_id)}`, h)}
          />
        ) : (
          <span className="text-xs text-muted-foreground">Admin only</span>
        )}
      </td>
    </tr>
  )
}

export function SelfhostTab() {
  const admin = useOperator()?.role === "admin"
  const q = useApiQuery<Inventory>(["selfhost"], "/v1/admin/selfhost/installations?limit=200&sort=newest", 5000)
  return (
    <div className="flex flex-col gap-4">
      {admin && <EnrolFlow />}
      {q.isError ? (
        <ErrorBlock what="installed nodes" error={q.error} retry={() => q.refetch()} />
      ) : !q.data ? (
        <Skeleton className="h-32 w-full" />
      ) : (
        <div data-table="installed nodes" data-state={q.data.installations.length === 0 ? "empty" : "ready"}>
          <div className="mb-2 flex gap-4 text-sm text-muted-foreground" data-installation-counts>
            <span>{q.data.counts.pending} waiting</span><span>{q.data.counts.enrolled} enrolled</span><span>{q.data.counts.ready} ready</span>
          </div>
          <div className="overflow-x-auto">
            <table className="w-full min-w-[40rem] border-collapse text-sm">
              <thead>
                <tr className="border-b border-divider text-left text-xs text-muted-foreground">
                  {["Node", "Runs", "State", "Scope", "Last heartbeat", "Actions"].map((h) => <th key={h} scope="col" className="h-9 px-3 font-medium">{h}</th>)}
                </tr>
              </thead>
              <tbody>{q.data.installations.map((i) => <Row key={i.installation_id} i={i} admin={admin} />)}</tbody>
            </table>
          </div>
          {q.data.installations.length === 0 && <p className="py-10 text-center text-sm text-muted-foreground">No self-hosted node is enrolled yet.</p>}
          {q.data.pagination.total > q.data.installations.length && <p className="text-sm text-muted-foreground">Showing the first {q.data.installations.length} of {q.data.pagination.total}.</p>}
        </div>
      )}
    </div>
  )
}
