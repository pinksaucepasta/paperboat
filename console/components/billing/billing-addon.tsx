"use client"

import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useApiQuery, type Workspace } from "@/lib/api"
import {
  addonNames,
  addonProduct,
  billingProductsQuery,
  billingQuery,
  formatPrice,
  setAddon,
  type Addon,
  type BillingWorkspace,
} from "@/lib/billing"
import { Row, Section } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { useTimeFormat } from "@/lib/use-time-format"
import { ErrorMessage } from "@/components/error-reference"
import { useShellState } from "@/lib/use-shell-state"

const kinds: Addon[] = ["sso", "scim", "sso_scim"]

/** The checkout consent of the Refund Policy (marketing legal/refunds, "Checkout consent"). */
const consentText =
  "Start the service now. I agree to the Terms of Service and the Refund Policy: the plan renews automatically until I cancel, cancelling stops the next renewal, and payments are not refunded. I ask for the service to start during the 14-day withdrawal period and understand that if I withdraw, I pay for the service up to that point."

/** Why the add-on cannot be changed right now, when it cannot. */
function blocked(billing: BillingWorkspace, date: (iso: string) => string): string | undefined {
  if (!billing.can_manage) return "Only the owner and billing admins can change the add-on."
  if (billing.state !== "active" && billing.state !== "trialing")
    return "The add-on can be changed while the plan is active."
  if (billing.cancel_at_period_end) return "The plan ends at the end of this period, so the add-on can't be changed."
  if (billing.scheduled_seats != null && billing.scheduled_seats_at)
    return `A seat reduction takes effect on ${date(billing.scheduled_seats_at)}. Change the add-on after that.`
}

/**
 * SSO (SAML) and SCIM are paid add-ons on the Team plan, at any team size: a second item on the team's subscription.
 * Adding or switching is charged now, prorated to the billing date; removing keeps it to the end of the paid period.
 */
export function Addons({ workspace, billing }: { workspace: Workspace; billing: BillingWorkspace }) {
  const time = useTimeFormat()
  const queryClient = useQueryClient()
  const products = useApiQuery(billingProductsQuery(workspace))
  const date = (iso: string) => time.date(iso, "date")
  const interval = billing.product?.interval ?? "month"
  const running: Addon = billing.addon && !billing.addon_until ? (billing.addon as Addon) : ""
  const [chosen, setChosen] = useShellState<Addon | undefined>("billing/billing-addon.tsx:chosen")
  const [agreed, setAgreed] = useShellState("billing/billing-addon.tsx:agreed", false)
  const selected = chosen ?? running
  const save = useMutation({
    mutationFn: () => setAddon(workspace, selected),
    onSuccess: () => {
      setChosen(undefined)
      setAgreed(false)
      return queryClient.invalidateQueries({ queryKey: billingQuery(workspace).queryKey })
    },
  })
  const reason = billing.granted ? "Subscribe to add SSO or SCIM." : blocked(billing, date)
  const charged = selected !== "" && selected !== running
  const price = selected ? addonProduct(products.data, selected, interval) : undefined
  const access = [billing.sso_addon && "SSO", billing.scim_addon && "SCIM"].filter(Boolean).join(" and ")
  return (
    <Section title="Single sign-on and directory sync">
      <Row label="Available">
        {access || "None"}
        {billing.addon && billing.addon_until && ` · ${addonNames[billing.addon]} ends ${date(billing.addon_until)}`}
      </Row>
      <div className="flex flex-wrap items-center gap-2 border-b border-divider py-3">
        {([...kinds, ""] as Addon[]).map((kind) => {
          const product = kind ? addonProduct(products.data, kind, interval) : undefined
          const label = addonNames[kind]
          return (
            <Button
              key={kind || "none"}
              variant={selected === kind ? "default" : "outline"}
              aria-pressed={selected === kind}
              disabled={Boolean(reason) || (kind !== "" && !product)}
              onClick={() => setChosen(kind)}
            >
              {product ? `${label} · ${formatPrice(product)}/${interval === "year" ? "yr" : "mo"}` : label}
            </Button>
          )
        })}
      </div>
      {reason && <p className="py-2 text-sm text-muted-foreground">{reason}</p>}
      {charged && (
        <label className="flex items-start gap-3 py-3 text-sm">
          <Checkbox checked={agreed} onCheckedChange={(on) => setAgreed(Boolean(on))} className="mt-0.5" />
          <span className="text-muted-foreground">{consentText}</span>
        </label>
      )}
      {selected !== running && !reason && (
        <div className="flex items-center justify-between gap-3 py-2 text-sm">
          <span className="text-muted-foreground">
            {charged
              ? `Charged now, prorated to the billing date${price ? `, then ${formatPrice(price)} each ${interval}` : ""}.`
              : `${addonNames[running]} keeps working until the end of the paid period. No credit is given.`}
          </span>
          <Button disabled={save.isPending || (charged && !agreed)} onClick={() => save.mutate()}>
            {!charged ? "Remove add-on" : running ? `Switch to ${addonNames[selected]}` : `Add ${addonNames[selected]}`}
          </Button>
        </div>
      )}
      {save.isError && (
        <p className="py-2 text-sm text-destructive">
          <ErrorMessage error={save.error} />
        </p>
      )}
    </Section>
  )
}
