"use client"

import { useMemo, useRef, useState } from "react"
import { ActionDialog } from "@/components/action-dialog"
import { RowMenu, type RowAction } from "@/components/row-menu"
import { DataTable } from "@/components/data-table"
import { Mono, StatusBadge } from "@/components/filters"
import { SelectField, TextField } from "@/components/fields"
import { Time } from "@/components/time"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { operatorAction, useOperator, type Data } from "@/lib/api"
import { useAllPages, useCursorList } from "@/lib/paging"
import { generateUsageKeyPair } from "@/lib/use-key-pair"

type Nodes = Data<"/v1/admin/nodes">
type Node = Nodes["items"][number]
type Regions = Data<"/v1/admin/regions">

const kinds: [string, string][] = [["edge", "Edge"], ["relay", "Relay"], ["both", "Edge and relay"]]

/** Roles and transports the server requires for each kind of node. */
function shape(kind: string) {
  const edge = kind !== "relay", relay = kind !== "edge"
  return { roles: [...(edge ? ["edge"] : []), ...(relay ? ["relay"] : [])], transports: [...(edge ? ["http3", "http2"] : []), ...(relay ? ["derp_quic", "derp_wss"] : [])] }
}

type Config = { region: string; pool: string; domain: string; host: string; tcp: string; quic: string; capacity: string; kind: string; usageKey: string }
const empty: Config = { region: "", pool: "", domain: "", host: "", tcp: "443", quic: "27446", capacity: "1000", kind: "edge", usageKey: "" }
const slug = /^[a-z][a-z0-9_-]{0,62}$/
/** The usage key is needed to enrol; an update keeps the enrolled one (the server holds it, write-only). */
const configValid = (c: Config, enrolling = true) => slug.test(c.region) && c.pool !== "" && c.domain !== "" && c.host !== "" && +c.tcp > 0 && +c.tcp < 65536 && +c.quic > 0 && +c.quic < 65536 && c.tcp !== c.quic && +c.capacity > 0 && (!enrolling || c.kind === "relay" || c.usageKey !== "")
const body = (c: Config, revision: number, withKey: boolean) => ({
  revision, region_id: c.region, edge_pool: c.pool, failure_domain: c.domain, endpoint_host: c.host, endpoint_tcp_port: +c.tcp, endpoint_quic_port: +c.quic,
  capacity_limit: +c.capacity, ...shape(c.kind), ...(withKey && c.usageKey ? { usage_public_key: c.usageKey } : {}),
})

function ConfigFields({ c, set, regions, keyHint, onSeed, lockKind }: { c: Config; set: (c: Config) => void; regions: string[]; keyHint?: React.ReactNode; onSeed?: (seed: string, key: string) => void; lockKind?: boolean }) {
  const [generated, setGenerated] = useState(false)
  return (
    <>
      <SelectField label="Kind" value={c.kind} onChange={(kind) => set({ ...c, kind })} options={kinds} disabled={lockKind} hint={lockKind ? "A relay can’t become an edge. Retire it and enrol an edge." : undefined} />
      <SelectField label="Region" value={c.region} onChange={(region) => set({ ...c, region })} options={[["", "Choose a region"], ...regions.map((r): [string, string] => [r, r])]} />
      <TextField label="Pool" value={c.pool} onChange={(pool) => set({ ...c, pool })} />
      <TextField label="Failure domain" value={c.domain} onChange={(domain) => set({ ...c, domain })} hint="What fails together: a rack, a provider zone." />
      <TextField label="Public hostname" value={c.host} onChange={(host) => set({ ...c, host })} />
      <div className="grid grid-cols-3 gap-2">
        <TextField label="TCP port" inputMode="numeric" value={c.tcp} onChange={(tcp) => set({ ...c, tcp })} />
        <TextField label="QUIC port" inputMode="numeric" value={c.quic} onChange={(quic) => set({ ...c, quic })} />
        <TextField label="Capacity" inputMode="numeric" value={c.capacity} onChange={(capacity) => set({ ...c, capacity })} />
      </div>
      {c.kind !== "relay" && (
        <>
          {!keyHint && <TextField label="Usage public key" value={c.usageKey} onChange={(usageKey) => set({ ...c, usageKey })} hint="Base64, 32 bytes. Can’t change after enrolment." />}
          {keyHint ?? (
            <Button
              type="button"
              variant="outline"
              size="sm"
              onClick={async () => {
                const pair = await generateUsageKeyPair()
                if (!pair) return window.alert("This browser cannot generate Ed25519 keys; paste a public key instead.")
                setGenerated(true)
                set({ ...c, usageKey: pair.publicKey })
                onSeed?.(pair.seed, pair.publicKey)
              }}
            >
              Generate key pair
            </Button>
          )}
          {generated && <span className="text-xs text-muted-foreground" data-key-generated>Generated. The private key is shown once after enrolling.</span>}
        </>
      )}
    </>
  )
}

/** Enrol a managed relay/edge: the server mints a runtime credential, shown once. */
export function EnrolManagedNode({ regions }: { regions: string[] }) {
  const [id, setId] = useState("")
  const [c, setC] = useState<Config>(empty)
  // The private half of a key pair made in this browser: shown once with the credential, then dropped.
  const seed = useRef<{ key: string; seed: string }>(undefined)
  return (
    <ActionDialog
      title="Enrol a managed node"
      description="You get its runtime credential, shown once."
      target={id || "-"}
      submitLabel="Enrol node"
      valid={/^[A-Za-z0-9_.:-]{1,128}$/.test(id) && configValid(c)}
      trigger={(open) => <Button size="lg" onClick={open}>Enrol a managed node</Button>}
      run={(h) => operatorAction<Data<"/v1/admin/nodes/{node_id}/enroll">>("POST", `/v1/admin/nodes/${encodeURIComponent(id)}/enroll`, h, body(c, 0, true))}
      secrets={(r) => {
        // The private seed is shown only for the public key that was actually enrolled.
        const shown = seed.current?.key === c.usageKey ? seed.current.seed : undefined
        seed.current = undefined
        return [{ label: "Runtime credential", value: (r as unknown as { runtime_credential: string }).runtime_credential }, ...(shown ? [{ label: "Usage signing key (private seed, base64url)", value: shown }] : [])]
      }}
      onDone={() => { setC(empty); setId("") }}
    >
      <TextField label="Node id" value={id} onChange={setId} placeholder="edge-hel-1" />
      <ConfigFields c={c} set={setC} regions={regions} onSeed={(v, key) => (seed.current = { key, seed: v })} />
    </ActionDialog>
  )
}

function nodeKind(n: Node): string {
  return n.roles.includes("edge") && n.roles.includes("relay") ? "both" : n.roles.includes("relay") ? "relay" : "edge"
}

function NodeActions({ node, regions, admin }: { node: Node; regions: string[]; admin: boolean }) {
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
  const [c, setC] = useState<Config>({ region: node.region_id, pool: node.edge_pool, domain: node.failure_domain, host: node.endpoint_host, tcp: String(node.endpoint_tcp_port), quic: String(node.endpoint_quic_port), capacity: String(node.capacity_limit), kind: nodeKind(node), usageKey: "" })
  if (!admin) return null
  const actions: RowAction[] = [
    {
      key: "edit",
      label: "Edit",
      dialog: (d) => (
        <ActionDialog
          {...d}
          title={`Update ${node.id}`}
          description="Deploy the new configuration with the returned generation; the old process is fenced."
          target={node.id}
          submitLabel="Save configuration"
          valid={configValid(c, false)}
          run={(h) => operatorAction("POST", path("update"), h, body(c, node.revision, false))}
        >
          <ConfigFields c={c} set={setC} regions={regions} lockKind={nodeKind(node) === "relay"} keyHint={<span className="text-xs text-muted-foreground">Usage key: kept from enrolment.</span>} />
        </ActionDialog>
      ),
    },
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
    {
      key: "rotate",
      label: "Rotate credential",
      destructive: true,
      dialog: (d) => (
        <ActionDialog
          {...d}
          title={`Rotate the credential of ${node.id}`}
          description="The old credential stops working now. Redeploy the node with the new one."
          target={node.id}
          destructive
          confirmWord={node.id}
          submitLabel="Rotate credential"
          run={(h) => operatorAction<{ runtime_credential: string }>("POST", path("rotate"), h, { revision: node.revision })}
          secrets={(r) => [{ label: "New runtime credential", value: r.runtime_credential }]}
        />
      ),
    },
    simple("retire", "Retire", `Retire ${node.id}`, node.carriers > 0 ? `${node.carriers} machine${node.carriers === 1 ? " is" : "s are"} still attached to it and will lose it now. Drain it and wait for 0 carriers first. Removes the node from the fleet for good.` : "Removes the node from the fleet for good.", true),
  ]
  return <RowMenu label={node.id} actions={actions} data-node-actions={node.id} />
}

export function ManagedNodes() {
  const admin = useOperator()?.role === "admin"
  const filters = useMemo(() => ({}), [])
  const list = useCursorList<Nodes>("/v1/admin/nodes", filters)
  const regionList = useAllPages<Regions["items"][number]>("/v1/admin/regions", (page) => (page as unknown as Regions).items)
  const regions = (regionList.data?.items ?? []).filter((r) => r.enabled).map((r) => r.id)
  // The display name users see for a region; the id when the list is not loaded.
  const regionName = new Map((regionList.data?.items ?? []).map((r) => [r.id, r.display_name]))
  return (
    <div className="flex flex-col gap-3">
      {admin && <div className="flex justify-end"><EnrolManagedNode regions={regions} /></div>}
      {regionList.isError && <p role="alert" className="text-sm text-destructive">Couldn’t load the regions, so enrolment can’t offer one. Reload to try again.</p>}
      {regionList.data?.capped && <p className="text-sm text-muted-foreground">Enrolment offers the first 1000 regions.</p>}
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
          { header: "Seen", cell: (n) => (n.observed_at ? <Time iso={n.observed_at} relative /> : "Waiting") },
          { header: "", className: "w-12", cell: (n) => <NodeActions node={n} regions={regions} admin={admin} /> },
        ]}
      />
    </div>
  )
}
