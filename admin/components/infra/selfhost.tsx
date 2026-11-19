"use client"

import { useMemo, useState } from "react"
import { ActionDialog } from "@/components/action-dialog"
import { CommandBlock } from "@/components/command-block"
import { Panel } from "@/components/page"
import { RowMenu, type DialogControl, type RowAction } from "@/components/row-menu"
import { ErrorBlock } from "@/components/error-block"
import { StatusBadge } from "@/components/filters"
import { SelectField, TextField } from "@/components/fields"
import { Time } from "@/components/time"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { PagerBar } from "@/components/data-table"
import { useCursorList } from "@/lib/paging"
import { operatorAction, useOperator, type Data } from "@/lib/api"
import { useNow } from "@/lib/use-now"

type Inventory = Data<"/v1/admin/selfhost/installations">
type Installation = Inventory["installations"][number]

const kinds: [string, string][] = [["relay", "Relay"], ["tunnel", "Edge (previews and tunnels)"], ["both", "Relay and edge"]]
const slug = /^[a-z][a-z0-9_-]{0,62}$/
const host = /^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$/

/** The command the operator runs on the new host; the one-time code is made there, by `pbh`, not by the server. */
export function installCommand(kind: string, endpointHost: string, region: string, name: string): string {
  const parts = ["sudo pbh install", `--${kind === "tunnel" ? "edge" : kind}`, `--endpoint-host ${endpointHost}`, `--region ${region}`]
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
    <Panel title="Enrol a node">
      <div className="flex flex-col gap-4" data-enrol-node>
        <div className="grid gap-3 md:grid-cols-2">
          <SelectField label="Runs" value={kind} onChange={setKind} options={kinds} />
          <TextField label="Public hostname or IP" value={endpoint} onChange={(v) => setEndpoint(v.trim())} placeholder="relay-1.example.test" />
          <TextField label="Region" value={region} onChange={setRegion} placeholder="hel" />
          <TextField label="Label" optional value={name} onChange={setName} maxLength={80} placeholder="Hetzner Helsinki" />
        </div>
        <div className="flex flex-col gap-1.5">
          <span className="text-sm font-medium">Run on the host</span>
          {command ? <CommandBlock command={command} data-install-command /> : <p className="rounded-lg bg-muted px-3.5 py-3 text-xs text-muted-foreground">Fill in the hostname and region.</p>}
          <span className="text-xs text-muted-foreground">Needs root, Docker and systemd. It prints a claim address and code.</span>
        </div>
        <ActionDialog
          title="Enrol the node"
          description="Paste the claim address and code the host printed."
          target={address || "-"}
          submitLabel="Enrol node"
          valid={address.length > 0 && /^pbh1\.[A-Za-z0-9_-]{43}\.[A-Za-z0-9_-]{43}$/.test(code)}
          trigger={(open) => <div><Button size="lg" disabled={!ready} onClick={() => { if (!address && endpoint) setAddress(`${endpoint}:8443`); open() }}>I ran it, enrol the node</Button></div>}
          run={(h) => operatorAction("POST", "/v1/admin/selfhost/enrollments", h, { address, code })}
          onDone={() => { setCode(""); setAddress("") }}
        >
          <TextField label="Claim address" value={address} onChange={(v) => setAddress(v.trim())} placeholder={endpoint ? `${endpoint}:8443` : "host:8443"} />
          <TextField label="Self-host code" value={code} onChange={(v) => setCode(v.trim())} type="password" autoComplete="off" hint="pbh1.… Used once." />
        </ActionDialog>
      </div>
    </Panel>
  )
}

const updateSteps: Record<string, string> = {
  requested: "waiting for check-in",
  downloading: "downloading",
  verifying: "verifying signature",
  swapping: "keeping current version",
  checking: "installing, health check",
  succeeded: "updated",
  rolled_back: "rolled back",
  failed: "update failed",
}

/** The node's release, how its last update went (live while it runs), and whether it is outdated or below the floor. */
function VersionCell({ i }: { i: Installation }) {
  const update = i.update
  const now = useNow()
  const recent = update && now - Date.parse(update.updated_at) < 86_400_000
  const failure = update && (update.state === "failed" || update.state === "rolled_back")
  return (
    <td data-node-version={i.version ?? ""}>
      <div className="tabular-nums">{i.version || "—"}</div>
      {update && recent ? (
        <div className={failure ? "text-xs text-destructive" : "text-xs text-muted-foreground"} title={failure ? `${update.detail ?? ""} ${update.support_reference ?? ""}`.trim() : undefined} data-node-update={update.state}>
          {update.target_version}: {updateSteps[update.state] ?? update.state}
          {failure && update.support_reference ? ` · ${update.support_reference}` : ""}
        </div>
      ) : i.update_required ? (
        <div className="text-xs text-destructive" title={i.update_required}>Update required</div>
      ) : i.update_available ? (
        <div className="text-xs text-muted-foreground">Outdated: {i.update_available} published</div>
      ) : null}
    </td>
  )
}

function Row({ i, admin }: { i: Installation; admin: boolean }) {
  const running = i.update && !["succeeded", "rolled_back", "failed"].includes(i.update.state)
  const target = i.update_available
  return (
    <tr className="[&>td]:h-14 [&>td]:px-3" data-installation={i.installation_id}>
      <td><div className="font-medium">{i.name || i.node_id}</div><div className="text-xs text-muted-foreground">{i.endpoint_host || "Address pending"}</div></td>
      <td className="first-letter:uppercase">{i.capability}</td>
      <td><StatusBadge state={i.enrollment_state === "pending" ? "waiting" : i.ready ? "ready" : "stale"} /></td>
      <td className="first-letter:uppercase">{i.scope_kind}</td>
      <VersionCell i={i} />
      <td>{i.last_heartbeat_at ? <Time iso={i.last_heartbeat_at} relative /> : "Waiting"}</td>
      <td>
        {admin && (
          <RowMenu
            label={i.name || i.node_id}
            data-installation-actions={i.installation_id}
            actions={[
              ...(target && !running
                ? ([
                    {
                      key: "update",
                      label: `Update to ${target}`,
                      dialog: (c: DialogControl) => (
                        <ActionDialog
                          {...c}
                          title={`Update ${i.name || i.node_id} to ${target}`}
                          description="The node checks in within a minute, verifies the signed release, keeps the current version, and rolls back on its own if the new one fails its health check. Progress shows in this row."
                          target={i.installation_id}
                          confirmWord={i.installation_id}
                          submitLabel="Update"
                          run={(h) => operatorAction("POST", `/v1/admin/selfhost/installations/${encodeURIComponent(i.installation_id)}/update`, h)}
                        />
                      ),
                    },
                  ] satisfies RowAction[])
                : []),
              {
                key: "remove",
                label: "Remove",
                destructive: true,
                dialog: (c) => (
                  <ActionDialog
                    {...c}
                    title={`Remove ${i.name || i.node_id}`}
                    description="It stops serving and its credentials are revoked. Uninstall it on the host afterwards."
                    target={i.installation_id}
                    destructive
                    confirmWord={i.installation_id}
                    submitLabel="Remove installation"
                    run={(h) => operatorAction("DELETE", `/v1/admin/selfhost/installations/${encodeURIComponent(i.installation_id)}`, h)}
                  />
                ),
              },
            ]}
          />
        )}
      </td>
    </tr>
  )
}

export function SelfhostTab() {
  const admin = useOperator()?.role === "admin"
  const filters = useMemo(() => ({ sort: "newest" }), [])
  const q = useCursorList<Inventory>("/v1/admin/selfhost/installations", filters, "cursor", 5000)
  return (
    <div className="flex flex-col gap-8">
      {admin && <EnrolFlow />}
      {q.isError ? (
        <ErrorBlock what="installed nodes" error={q.error} retry={() => q.refetch()} />
      ) : !q.data ? (
        <Skeleton className="h-32 w-full" />
      ) : (
        <div data-table="installed nodes" data-state={q.data.installations.length === 0 ? "empty" : "ready"}>
          <div className="mb-3 flex gap-4 text-sm text-muted-foreground tabular-nums" data-installation-counts>
            <span>{q.data.counts.pending} waiting</span><span>{q.data.counts.enrolled} enrolled</span><span>{q.data.counts.ready} ready</span>
          </div>
          <div className="overflow-x-auto">
            <table className="w-full min-w-[40rem] border-collapse text-sm">
              <thead>
                <tr className="border-b border-divider text-left text-xs text-muted-foreground">
                  {["Node", "Runs", "State", "Scope", "Version", "Seen", ""].map((h) => <th key={h} scope="col" className="h-9 px-3 font-normal">{h}</th>)}
                </tr>
              </thead>
              <tbody className="divide-y divide-divider">{q.data.installations.map((i) => <Row key={i.installation_id} i={i} admin={admin} />)}</tbody>
            </table>
          </div>
          {q.data.installations.length === 0 && <p className="py-12 text-center text-sm text-muted-foreground">No nodes yet.</p>}
          <PagerBar page={q.page} count={q.data.installations.length} total={q.data.pagination.total} hasNext={q.hasNext} go={q.go} />
        </div>
      )}
    </div>
  )
}
