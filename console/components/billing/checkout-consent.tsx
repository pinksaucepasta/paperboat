"use client"

import { Checkbox } from "@/components/ui/checkbox"

/**
 * The consent every purchase needs before it reaches Stripe (marketing /legal/refunds): no refunds, and an
 * express request to start during the EU/UK 14-day withdrawal period (Directive 2011/83/EU art. 7(3), 14(3)).
 */
export function CheckoutConsent({ checked, onChange }: { checked: boolean; onChange: (checked: boolean) => void }) {
  return (
    <label className="flex items-start gap-2 py-2 text-xs text-muted-foreground">
      <Checkbox className="mt-0.5" checked={checked} onCheckedChange={(on) => onChange(Boolean(on))} />
      <span>
        Start the service now. I agree to the Terms of Service and the Refund Policy: the plan renews automatically until I
        cancel, cancelling stops the next renewal, and payments are not refunded. I ask for the service to start during the
        14-day withdrawal period and understand that if I withdraw, I pay for the service up to that point.
      </span>
    </label>
  )
}
