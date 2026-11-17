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
  if (!admin) return <p className="text-sm text-muted-foreground">Key management needs an administrator.</p>
  return (
    <div className="flex flex-col gap-8" data-keys>
      <section className="flex max-w-xl flex-col gap-3">
        <h2 className="text-sm font-medium">Edge usage keys</h2>
        <p className="text-sm text-muted-foreground">An edge signs its traffic reports with a usage key the server verifies. Provision a key for an edge, or revoke one (its reports stop being accepted).</p>
        <TextField label="Key id" value={id} onChange={(v) => setId(v.trim())} />
        <TextField label="Edge node id" value={edge} onChange={(v) => setEdge(v.trim())} hint="Needed to provision." />
        <TextField label="Public key (base64 or base64url)" value={publicKey} onChange={(v) => setPublicKey(v.trim())} hint="Needed to provision." />
        <div className="grid grid-cols-2 gap-2">
          <TextField label="Valid from" type="datetime-local" value={from} onChange={setFrom} />
          <TextField label="Valid until" type="datetime-local" value={until} onChange={setUntil} />
        </div>
        <TextField label="Revocation reason" value={reason} onChange={setReason} hint="Needed to revoke; stored with the revocation." />
        <div className="flex gap-2">
          <ActionDialog
            title={`Provision usage key ${id}`}
            description="Registers a public key the server will accept usage reports signed by, for this edge and period."
            target={id}
            submitLabel="Provision key"
            valid={keyId.test(id) && edge !== "" && publicKey !== "" && iso(from) !== "" && iso(until) !== ""}
            trigger={(open) => <Button disabled={!keyId.test(id) || !edge || !publicKey || !iso(from) || !iso(until)} onClick={open}>Provision key</Button>}
            run={(h) => operatorAction("POST", "/v1/admin/edge/usage-keys", h, { key_id: id, edge_node_id: edge, public_key: toUrl(publicKey), not_before: iso(from), expires_at: iso(until) })}
            onDone={() => setPublicKey("")}
          />
          <ActionDialog
            title={`Revoke usage key ${id}`}
            description="The server stops accepting usage reports signed with this key. This cannot be undone: provision a new key for the edge."
            target={id}
            destructive
            confirmWord={id}
            submitLabel="Revoke key"
            valid={keyId.test(id) && reason.length > 0}
            trigger={(open) => <Button variant="destructive" disabled={!keyId.test(id) || !reason} onClick={open}>Revoke key</Button>}
            run={(h) => operatorAction("POST", `/v1/admin/edge/usage-keys/${encodeURIComponent(id)}/revoke`, h, { reason })}
          />
        </div>
      </section>
      <section className="flex max-w-xl flex-col gap-3">
        <h2 className="text-sm font-medium">Mint signing keys</h2>
        <p className="text-sm text-muted-foreground">Revoking a signing key withdraws it from every trust document: credentials signed with it stop working everywhere.</p>
        <TextField label="Key id" value={mintId} onChange={(v) => setMintId(v.trim())} />
        <TextField label="Revocation reason" value={mintReason} onChange={setMintReason} />
        <div>
          <ActionDialog
            title={`Revoke signing key ${mintId}`}
            description="Withdraws the key from all trust documents. Every credential it signed stops being accepted. This cannot be undone."
            target={mintId}
            destructive
            confirmWord={mintId}
            submitLabel="Revoke signing key"
            valid={keyId.test(mintId) && mintReason.length > 0}
            trigger={(open) => <Button variant="destructive" disabled={!keyId.test(mintId) || !mintReason} onClick={open}>Revoke signing key</Button>}
            run={(h) => operatorAction("POST", `/v1/admin/mint/signing-keys/${encodeURIComponent(mintId)}/revoke`, h, { reason: mintReason })}
          />
        </div>
      </section>
    </div>
  )
}
