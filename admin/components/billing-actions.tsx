"use client"

import { useState } from "react"
import { ActionDialog } from "@/components/action-dialog"
import { ErrorBlock } from "@/components/error-block"
import { SelectField, TextField } from "@/components/fields"
import { Facts, Panel } from "@/components/page"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Skeleton } from "@/components/ui/skeleton"
import { formatBytes } from "@/lib/format"
import { operatorAction, useApiQuery, useOperator, type Data } from "@/lib/api"

type Product = Data<"/v1/admin/billing/products">["products"][number]
const codePattern = /^[a-z][a-z0-9_-]{1,63}$/
/** The one top-up unit the server accepts (shared constant on the server: billing.TopupUnitBytes). */
const topUpUnitBytes = 100_000_000_000
const catalogTypes: [string, string][] = [["personal_subscription", "Personal subscription"], ["team_subscription", "Team subscription"], ["top_up", "Extra bandwidth top-up"], ["team_addon", "Team SSO / SCIM add-on"]]

/** A non-negative whole number of bytes from a text field; "" means none (null). */
const bytesOrNull = (text: string) => (text.trim() === "" ? null : Number(text))
const bytesValid = (text: string) => text.trim() === "" || (/^\d{1,15}$/.test(text.trim()) && Number.isSafeInteger(Number(text)))

/** Create a catalog product (no `product`) or change one (the Stripe price and type are fixed after creation). */
export function ProductEditor({ product }: { product?: Product }) {
  const [code, setCode] = useState(product?.code ?? "")
  const [name, setName] = useState(product?.display_name ?? "")
  const [price, setPrice] = useState(product?.price_id ?? "")
  const [type, setType] = useState(product?.catalog_type ?? "personal_subscription")
  const [active, setActive] = useState(product?.active ?? false)
  const [allowance, setAllowance] = useState(product?.allowance_bytes == null ? "" : String(product.allowance_bytes))
  const topUp = type === "top_up"
  const valid = codePattern.test(code) && name.trim() !== "" && name.length <= 120 && price.trim() !== "" && (topUp || bytesValid(allowance))
  return (
    <ActionDialog
      title={product ? `Edit ${product.code}` : "Add a product"}
      description="The Stripe price is checked live before saving."
      target={code || "-"}
      submitLabel={product ? "Save product" : "Add product"}
      valid={valid}
      trigger={(open) => <Button size={product ? "default" : "lg"} variant="outline" onClick={open} data-product-action={product?.code ?? "new"}>{product ? "Edit" : "Add product"}</Button>}
      run={(h) =>
        operatorAction("PUT", `/v1/admin/billing/products/${encodeURIComponent(code)}`, h, {
          version: product?.version ?? 0,
          display_name: name.trim(),
          provider_price_id: price.trim(),
          catalog_type: type,
          active,
          allowance_bytes: topUp ? null : bytesOrNull(allowance),
          bytes_amount: topUp ? topUpUnitBytes : null,
        })
      }
    >
      <TextField label="Code" value={code} onChange={setCode} disabled={Boolean(product)} hint={product ? undefined : "Lowercase letters, digits, _ and -. Can’t change later."} />
      <TextField label="Display name" value={name} onChange={setName} maxLength={120} />
      <TextField label="Stripe price id" value={price} onChange={setPrice} disabled={Boolean(product)} placeholder="price_…" hint={product ? undefined : "Can’t change later: add a new plan for a new price."} />
      <SelectField label="Type" value={type} onChange={setType} options={catalogTypes} disabled={Boolean(product)} />
      {!topUp && <TextField label="Allowance (bytes)" value={allowance} onChange={setAllowance} inputMode="numeric" hint={bytesValid(allowance) && allowance.trim() ? formatBytes(Number(allowance)) : "Empty for none"} />}
      {topUp && <p className="text-xs text-muted-foreground">Adds {formatBytes(topUpUnitBytes)} per unit.</p>}
      <label className="flex items-center gap-2 text-sm">
        <Checkbox checked={active} onCheckedChange={(v) => setActive(v === true)} />
        On sale
      </label>
    </ActionDialog>
  )
}

/** The monthly allowances every personal and team workspace gets; one revision-fenced setting. */
export function BillingPolicy() {
  const admin = useOperator()?.role === "admin"
  const q = useApiQuery<Data<"/v1/admin/config/billing">>(["billing-policy"], "/v1/admin/config/billing")
  const [personal, setPersonal] = useState("")
  const [team, setTeam] = useState("")
  if (q.isError) return <ErrorBlock what="the billing policy" error={q.error} retry={() => q.refetch()} />
  if (!q.data) return <Skeleton className="h-16 w-full" />
  const policy = q.data
  const show = (v: number | null) => (v === null ? "None" : formatBytes(v))
  return (
    <div data-billing-policy data-revision={policy.revision}>
    <Panel
      title="Monthly allowances"
      action={admin && (
        <ActionDialog
          title="Change the monthly allowances"
          description="Applies from the next allowance month. Empty for none."
          target="billing-policy"
          submitLabel="Save allowances"
          valid={bytesValid(personal) && bytesValid(team)}
          trigger={(open) => (
            <Button
              variant="outline"
              size="lg"
              onClick={() => {
                setPersonal(policy.personal_allowance_bytes === null ? "" : String(policy.personal_allowance_bytes))
                setTeam(policy.team_allowance_bytes === null ? "" : String(policy.team_allowance_bytes))
                open()
              }}
            >
              Change
            </Button>
          )}
          run={(h) => operatorAction("PUT", "/v1/admin/config/billing", h, { personal_allowance_bytes: bytesOrNull(personal), team_allowance_bytes: bytesOrNull(team), revision: policy.revision })}
        >
          <TextField label="Personal allowance (bytes)" value={personal} onChange={setPersonal} inputMode="numeric" />
          <TextField label="Team allowance (bytes)" value={team} onChange={setTeam} inputMode="numeric" />
        </ActionDialog>
      )}
    >
      <Facts
        rows={[
          ["Personal", show(policy.personal_allowance_bytes)],
          ["Team", show(policy.team_allowance_bytes)],
          ["Revision", policy.revision],
        ]}
      />
    </Panel>
    </div>
  )
}

// The server's pattern (billing/types.go): starts alphanumeric, then letters, digits and : . _ / -, 8–256 long.
const evidenceOk = (v: string) => /^[A-Za-z0-9][A-Za-z0-9:._/-]{7,255}$/.test(v)

/**
 * Recovery of a billing operation whose outcome is uncertain: the operator records what Stripe shows, with an
 * evidence reference (never credentials). Only offered while the operation is `uncertain`; the server refuses
 * while the safe retry window is still open and says so.
 */
export function OperationActions({ id, state }: { id: string; state: string }) {
  const role = useOperator()?.role
  const [evidence, setEvidence] = useState("")
  const [session, setSession] = useState("")
  if (state !== "uncertain" || (role !== "admin" && role !== "billing")) return null
  const path = (a: string) => `/v1/admin/billing/workspace-operations/${encodeURIComponent(id)}/${a}`
  const evidenceField = <TextField label="Evidence reference" value={evidence} onChange={setEvidence} maxLength={256} hint="Stripe event or incident id. No payment data." />
  return (
    <div className="flex flex-wrap justify-end gap-1.5" data-operation-actions={id}>
      <ActionDialog
        title="Retire as not applied"
        description="Marks it as not applied at Stripe. Check Stripe shows no charge first."
        target={id}
        destructive
        confirmWord={id}
        submitLabel="Retire operation"
        valid={evidenceOk(evidence)}
        trigger={(open) => <Button variant="outline" onClick={open}>Retire</Button>}
        run={(h) => operatorAction("POST", path("retire"), h, { outcome: "not_applied", evidence_reference: evidence })}
      >
        {evidenceField}
      </ActionDialog>
      <ActionDialog
        title="Reconcile checkout"
        description="Binds the completed Stripe checkout so the workspace gets what was bought."
        target={id}
        submitLabel="Reconcile"
        valid={evidenceOk(evidence) && session.trim() !== ""}
        trigger={(open) => <Button variant="outline" onClick={open}>Reconcile</Button>}
        run={(h) => operatorAction("POST", path("reconcile-checkout"), h, { provider_session_id: session.trim(), evidence_reference: evidence })}
      >
        <TextField label="Stripe checkout session id" value={session} onChange={setSession} placeholder="cs_…" />
        {evidenceField}
      </ActionDialog>
      <ActionDialog
        title="Supersede billing portal link"
        description="Invalidates the portal link; the customer opens a new one."
        target={id}
        destructive
        confirmWord={id}
        submitLabel="Supersede"
        valid={evidenceOk(evidence)}
        trigger={(open) => <Button variant="outline" onClick={open}>Supersede portal</Button>}
        run={(h) => operatorAction("POST", path("supersede-portal"), h, { evidence_reference: evidence })}
      >
        {evidenceField}
      </ActionDialog>
    </div>
  )
}
