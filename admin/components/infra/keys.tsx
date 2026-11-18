"use client"

import { useMemo, useState } from "react"
import { ActionDialog } from "@/components/action-dialog"
import { DataTable } from "@/components/data-table"
import { ErrorBlock } from "@/components/error-block"
import { TextField } from "@/components/fields"
import { StatusBadge } from "@/components/filters"
import { Panel } from "@/components/page"
import { RowMenu } from "@/components/row-menu"
import { Time } from "@/components/time"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { operatorAction, useApiQuery, useOperator, type Data } from "@/lib/api"
import { useCursorList } from "@/lib/paging"
import { useNow } from "@/lib/use-now"

type UsageKeys = Data<"/v1/admin/edge/usage-keys">
type SigningKeys = Data<"/v1/admin/mint/signing-keys">

const keyId = /^[A-Za-z0-9_.:-]{1,128}$/
/** The server wants base64url without padding; a key copied as standard base64 is converted. */
const toUrl = (key: string) => key.replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "")
/** A datetime-local value as RFC 3339 UTC, "" when empty or unparsable. */
const iso = (local: string) => (Number.isNaN(Date.parse(local)) ? "" : new Date(Date.parse(local)).toISOString())

function ProvisionUsageKey() {
  const [id, setId] = useState("")
  const [edge, setEdge] = useState("")
  const [publicKey, setPublicKey] = useState("")
  const [from, setFrom] = useState("")
  const [until, setUntil] = useState("")
  return (
    <ActionDialog
      title="Provision a usage key"
      description="The server accepts this edge’s usage reports signed with it, for this period."
      target={id || "-"}
      submitLabel="Provision key"
      valid={keyId.test(id) && edge !== "" && publicKey !== "" && iso(from) !== "" && iso(until) !== ""}
      trigger={(open) => <Button variant="outline" size="lg" onClick={open}>Provision key</Button>}
      run={(h) => operatorAction("POST", "/v1/admin/edge/usage-keys", h, { key_id: id, edge_node_id: edge, public_key: toUrl(publicKey), not_before: iso(from), expires_at: iso(until) })}
      onDone={() => setPublicKey("")}
    >
      <TextField label="Key id" value={id} onChange={(v) => setId(v.trim())} />
      <TextField label="Edge node id" value={edge} onChange={(v) => setEdge(v.trim())} />
      <TextField label="Public key" value={publicKey} onChange={(v) => setPublicKey(v.trim())} hint="Base64 or base64url." />
      <div className="grid grid-cols-2 gap-2">
        <TextField label="Valid from" type="datetime-local" value={from} onChange={setFrom} />
        <TextField label="Valid until" type="datetime-local" value={until} onChange={setUntil} />
      </div>
    </ActionDialog>
  )
}

/** Revoke one key with a reason: the dialog is shared by usage and signing keys. */
function RevokeMenu({ id, path, title, description, attr }: { id: string; path: string; title: string; description: string; attr: Record<`data-${string}`, string> }) {
  const [reason, setReason] = useState("")
  return (
    <RowMenu
      label={id}
      {...attr}
      actions={[
        {
          key: "revoke",
          label: "Revoke",
          destructive: true,
          dialog: (c) => (
            <ActionDialog {...c} title={title} description={description} target={id} destructive confirmWord={id} submitLabel="Revoke key" valid={reason.trim().length > 0} run={(h) => operatorAction("POST", path, h, { reason })}>
              <TextField label="Revocation reason" value={reason} onChange={setReason} maxLength={512} />
            </ActionDialog>
          ),
        },
      ]}
    />
  )
}

const usageState = (k: UsageKeys["items"][number], now: number) => (k.revoked_at ? "revoked" : !now ? "active" : Date.parse(k.expires_at) <= now ? "expired" : Date.parse(k.not_before) > now ? "pending" : "active")
const signingState = (k: SigningKeys["items"][number]) => (k.revoked_at ? "revoked" : k.active ? "signing" : k.withdrawn ? "withdrawn" : "published")

/** Edge usage keys and mint signing keys, each a list whose rows can be revoked. */
export function Keys() {
  const admin = useOperator()?.role === "admin"
  const filters = useMemo(() => ({}), [])
  const usage = useCursorList<UsageKeys>("/v1/admin/edge/usage-keys", filters)
  const signing = useApiQuery<SigningKeys>(["signing-keys"], admin ? "/v1/admin/mint/signing-keys" : null)
  const now = useNow()
  if (!admin) return <p className="py-12 text-center text-sm text-muted-foreground">Admins only.</p>
  return (
    <div className="flex flex-col gap-8" data-keys>
      <Panel title="Edge usage keys" action={<ProvisionUsageKey />}>
        <DataTable
          what="usage keys"
          list={usage}
          rows={(d) => d.items}
          rowKey={(k) => k.key_id}
          empty="No usage keys yet."
          columns={[
            { header: "Key", cell: (k) => <span className="font-medium">{k.key_id}</span> },
            { header: "Edge", className: "hidden md:table-cell", cell: (k) => k.edge_node_id },
            { header: "State", cell: (k) => <StatusBadge state={usageState(k, now)} /> },
            { header: "Valid until", className: "hidden md:table-cell", cell: (k) => <Time iso={k.expires_at} /> },
            {
              header: "",
              className: "w-12",
              cell: (k) =>
                k.revoked_at ? null : (
                  <RevokeMenu id={k.key_id} path={`/v1/admin/edge/usage-keys/${encodeURIComponent(k.key_id)}/revoke`} title={`Revoke usage key ${k.key_id}`} description="Reports signed with it stop being accepted. Can’t be undone." attr={{ "data-usage-key-actions": k.key_id }} />
                ),
            },
          ]}
        />
      </Panel>
      <Panel title="Mint signing keys">
        {signing.isError ? (
          <ErrorBlock what="signing keys" error={signing.error} retry={() => signing.refetch()} />
        ) : (
          <table className="w-full border-collapse text-sm" data-table="signing keys">
            <thead>
              <tr className="border-b border-divider text-left text-xs text-muted-foreground">
                {["Key", "State", "Revoked", ""].map((h) => (
                  <th key={h} scope="col" className="h-9 px-3 font-normal">{h}</th>
                ))}
              </tr>
            </thead>
            <tbody className="divide-y divide-divider">
              {!signing.data
                ? Array.from({ length: 2 }, (_, i) => (
                    <tr key={i} className="[&>td]:h-14 [&>td]:px-3">
                      <td><Skeleton className="h-3 w-24" /></td>
                      <td><Skeleton className="h-3 w-16" /></td>
                      <td />
                      <td />
                    </tr>
                  ))
                : signing.data.items.map((k) => (
                    <tr key={k.key_id} className="[&>td]:h-14 [&>td]:px-3">
                      <td className="font-medium">{k.key_id}</td>
                      <td><StatusBadge state={signingState(k)} /></td>
                      <td className="text-muted-foreground">{k.revoked_at ? <><Time iso={k.revoked_at} relative />{k.reason && ` · ${k.reason}`}</> : "—"}</td>
                      <td className="w-12">
                        {!k.active && !k.revoked_at && (
                          <RevokeMenu id={k.key_id} path={`/v1/admin/mint/signing-keys/${encodeURIComponent(k.key_id)}/revoke`} title={`Revoke signing key ${k.key_id}`} description="Every credential it signed stops working everywhere. Can’t be undone." attr={{ "data-signing-key-actions": k.key_id }} />
                        )}
                      </td>
                    </tr>
                  ))}
            </tbody>
          </table>
        )}
        {signing.data?.items.length === 0 && <p className="py-12 text-center text-sm text-muted-foreground">No signing keys configured.</p>}
      </Panel>
    </div>
  )
}
