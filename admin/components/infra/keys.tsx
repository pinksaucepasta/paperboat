"use client"

import { useState } from "react"
import { ActionDialog } from "@/components/action-dialog"
import { TextField } from "@/components/fields"
import { Button } from "@/components/ui/button"
import { operatorAction, useOperator } from "@/lib/api"

const keyId = /^[A-Za-z0-9_.:-]{1,128}$/
/** The server wants base64url without padding; a key copied as standard base64 is converted. */
const toUrl = (key: string) => key.replaceAll("+", "-").replaceAll("/", "_").replace(/=+$/, "")
/** A datetime-local value as RFC 3339 UTC, "" when empty or unparsable. */
const iso = (local: string) => (Number.isNaN(Date.parse(local)) ? "" : new Date(Date.parse(local)).toISOString())

/** Edge usage keys (provision, revoke) and mint signing keys (revoke). There is no list of keys: name them. */
export function Keys() {
  const admin = useOperator()?.role === "admin"
  const [id, setId] = useState("")
  const [edge, setEdge] = useState("")
  const [publicKey, setPublicKey] = useState("")
  const [from, setFrom] = useState("")
  const [until, setUntil] = useState("")
  const [reason, setReason] = useState("")
  const [mintId, setMintId] = useState("")
  const [mintReason, setMintReason] = useState("")
  if (!admin) return <p className="py-12 text-center text-sm text-muted-foreground">Admins only.</p>
  return (
    <div className="flex flex-col gap-8" data-keys>
      <section className="flex max-w-md flex-col gap-3">
        <h2 className="text-sm font-medium">Edge usage keys</h2>
        <TextField label="Key id" value={id} onChange={(v) => setId(v.trim())} />
        <TextField label="Edge node id" value={edge} onChange={(v) => setEdge(v.trim())} />
        <TextField label="Public key" value={publicKey} onChange={(v) => setPublicKey(v.trim())} hint="Base64 or base64url." />
        <div className="grid grid-cols-2 gap-2">
          <TextField label="Valid from" type="datetime-local" value={from} onChange={setFrom} />
          <TextField label="Valid until" type="datetime-local" value={until} onChange={setUntil} />
        </div>
        <TextField label="Revocation reason" value={reason} onChange={setReason} hint="Only to revoke." />
        <div className="flex gap-2 pt-1">
          <ActionDialog
            title={`Provision usage key ${id}`}
            description="The server accepts this edge’s usage reports signed with it, for this period."
            target={id}
            submitLabel="Provision key"
            valid={keyId.test(id) && edge !== "" && publicKey !== "" && iso(from) !== "" && iso(until) !== ""}
            trigger={(open) => <Button size="lg" disabled={!keyId.test(id) || !edge || !publicKey || !iso(from) || !iso(until)} onClick={open}>Provision key</Button>}
            run={(h) => operatorAction("POST", "/v1/admin/edge/usage-keys", h, { key_id: id, edge_node_id: edge, public_key: toUrl(publicKey), not_before: iso(from), expires_at: iso(until) })}
            onDone={() => setPublicKey("")}
          />
          <ActionDialog
            title={`Revoke usage key ${id}`}
            description="Reports signed with it stop being accepted. Can’t be undone."
            target={id}
            destructive
            confirmWord={id}
            submitLabel="Revoke key"
            valid={keyId.test(id) && reason.length > 0}
            trigger={(open) => <Button size="lg" variant="outline" disabled={!keyId.test(id) || !reason} onClick={open}>Revoke key</Button>}
            run={(h) => operatorAction("POST", `/v1/admin/edge/usage-keys/${encodeURIComponent(id)}/revoke`, h, { reason })}
          />
        </div>
      </section>
      <section className="flex max-w-md flex-col gap-3">
        <h2 className="text-sm font-medium">Mint signing keys</h2>
        <TextField label="Key id" value={mintId} onChange={(v) => setMintId(v.trim())} />
        <TextField label="Revocation reason" value={mintReason} onChange={setMintReason} />
        <div>
          <ActionDialog
            title={`Revoke signing key ${mintId}`}
            description="Every credential it signed stops working everywhere. Can’t be undone."
            target={mintId}
            destructive
            confirmWord={mintId}
            submitLabel="Revoke signing key"
            valid={keyId.test(mintId) && mintReason.length > 0}
            trigger={(open) => <Button size="lg" variant="outline" disabled={!keyId.test(mintId) || !mintReason} onClick={open}>Revoke signing key</Button>}
            run={(h) => operatorAction("POST", `/v1/admin/mint/signing-keys/${encodeURIComponent(mintId)}/revoke`, h, { reason: mintReason })}
          />
        </div>
      </section>
    </div>
  )
}
