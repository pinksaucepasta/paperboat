"use client"

import { Checkbox } from "@/components/ui/checkbox"

/**
 * The consent every purchase needs before it reaches Stripe; its text is quoted on marketing /legal/refunds.
 */
export function CheckoutConsent({ checked, onChange }: { checked: boolean; onChange: (checked: boolean) => void }) {
  return (
    <label className="flex items-start gap-2 py-2 text-xs text-muted-foreground">
      <Checkbox className="mt-0.5" checked={checked} onCheckedChange={(on) => onChange(Boolean(on))} />
      <span>
        I agree to the Terms of Service and the Refund Policy: the plan renews automatically until I cancel, cancelling stops
        the next renewal, and payments are not refunded.
      </span>
    </label>
  )
}
