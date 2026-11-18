"use client"

import { useState } from "react"
import { ActionDialog } from "@/components/action-dialog"
import { SelectField, TextField } from "@/components/fields"
import { Facts, Panel } from "@/components/page"
import { StatusBadge } from "@/components/filters"
import { Time } from "@/components/time"
import { Button } from "@/components/ui/button"
import { useAllPages } from "@/lib/paging"
import { ApiError, operatorAction, useApiQuery, useOperator, type Data } from "@/lib/api"

type Workspace = Data<"/v1/admin/billing/workspaces/{workspace_id}">
type Products = Data<"/v1/admin/billing/products">

/**
 * The plan of one account or team, and the support comp: grant a plan for a number of days (it ends by itself) or
 * revoke a grant. The workspace id is personal:<account> or team:<team>. A workspace the billing page has never
 * opened has no row yet (404): it is a free workspace at revision 1.
 */
export function PlanGrant({ scope, id }: { scope: "personal" | "team"; id: string }) {
  const admin = useOperator()?.role === "admin"
  const workspaceId = `${scope}:${id}`
  const workspace = useApiQuery<Workspace>(["workspace", workspaceId], admin ? `/v1/admin/billing/workspaces/${encodeURIComponent(workspaceId)}` : null)
  const products = useAllPages<Products["products"][number]>(admin ? "/v1/admin/billing/products" : null, (page) => (page as unknown as Products).products)
  const choices = (products.data?.items ?? []).filter((p) => p.active && p.catalog_type === `${scope}_subscription`)
  const [code, setCode] = useState("")
  const [days, setDays] = useState("30")
  const [seats, setSeats] = useState("5")
  if (!admin) return null
  const missing = workspace.error instanceof ApiError && workspace.error.status === 404
  const w = missing ? undefined : workspace.data
  const revision = w?.revision ?? 1
  const product = code || choices[0]?.code || ""
  const daysOk = /^\d{1,3}$/.test(days) && +days >= 1 && +days <= 366
  const seatsOk = scope === "personal" || (/^\d{1,4}$/.test(seats) && +seats >= 1 && +seats <= 128)
  return (
    <Panel
      title="Plan"
      action={
        <div className="flex items-center gap-2" data-plan-actions>
          {workspace.isError && !missing && <span className="text-xs text-destructive">Couldn’t load the plan.</span>}
        <ActionDialog
          title={`Grant a plan to ${workspaceId}`}
          description="A plan without Stripe that ends by itself. Refused while a Stripe subscription or checkout is live."
          target={workspaceId}
          submitLabel="Grant plan"
          valid={product !== "" && daysOk && seatsOk}
          trigger={(open) => <Button variant="outline" size="lg" onClick={open} data-action="grant-plan">Grant a plan</Button>}
          run={(h) => operatorAction("POST", `/v1/admin/billing/workspaces/${encodeURIComponent(workspaceId)}/grant`, h, { revision, product_code: product, days: +days, ...(scope === "team" ? { quantity: +seats } : {}) })}
        >
          <SelectField label="Plan" value={product} onChange={setCode} options={choices.length ? choices.map((p): [string, string] => [p.code, `${p.display_name} (${p.code})`]) : [["", products.isError ? "Couldn’t load plans" : products.isPending ? "Loading…" : "No active plan"]]} hint={products.data?.capped ? "Showing the first 1000 plans." : undefined} />
          <TextField label="Days" value={days} onChange={setDays} inputMode="numeric" hint="1–366" />
          {scope === "team" && <TextField label="Seats" value={seats} onChange={setSeats} inputMode="numeric" hint="1–128" />}
        </ActionDialog>
        {w?.granted && (
          <ActionDialog
            title={`Revoke the grant of ${workspaceId}`}
            description="Ends the granted plan now. Stripe subscriptions are cancelled in Stripe."
            target={workspaceId}
            destructive
            confirmWord={workspaceId}
            submitLabel="Revoke grant"
            trigger={(open) => <Button variant="outline" size="lg" onClick={open} data-action="revoke-grant">Revoke grant</Button>}
            run={(h) => operatorAction("POST", `/v1/admin/billing/workspaces/${encodeURIComponent(workspaceId)}/revoke`, h, { revision })}
          />
        )}
        </div>
      }
    >
      <Facts
        rows={[
          ["Plan", w?.product_code ? <span className="inline-flex items-center gap-3">{w.product_code}<StatusBadge state={w.state} />{w.granted && <span className="text-xs text-muted-foreground" data-plan-granted>Granted</span>}</span> : "Free"],
          ["Ends", w?.period_end ? <Time iso={w.period_end} /> : "—"],
          ...(scope === "team" && w ? [["Seats", w.quantity] as [string, React.ReactNode]] : []),
        ]}
      />
    </Panel>
  )
}
