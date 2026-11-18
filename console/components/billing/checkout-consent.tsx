"use client"

import { Checkbox } from "@/components/ui/checkbox"
import { clientConfigQuery, useApiQuery } from "@/lib/api"

/**
 * The consent every purchase needs before it reaches Stripe (marketing /legal/refunds): no refunds, and an
 * express request to start during the EU/UK 14-day withdrawal period (Directive 2011/83/EU art. 7(3), 14(3)).
 * The server owns the wording and its version (client configuration `purchase_consent`); `terms` is the version
 * the buyer ticked, sent with the purchase, or null while unticked.
 */
export function CheckoutConsent({ terms, onChange }: { terms: string | null; onChange: (terms: string | null) => void }) {
  const consent = useApiQuery(clientConfigQuery).data?.purchase_consent
  return (
    <label className="flex items-start gap-2 py-2 text-xs text-muted-foreground">
      <Checkbox
        className="mt-0.5"
        disabled={!consent}
        checked={terms !== null}
        onCheckedChange={(on) => onChange(on && consent ? consent.version : null)}
      />
      <span>{consent?.text ?? "Loading the terms…"}</span>
    </label>
  )
}
