"use client"

import { Checkbox } from "@/components/ui/checkbox"

/**
 * The consent every purchase needs before it reaches Stripe (marketing /legal/refunds): the service
 * starts at once, so the buyer gives up the 14-day withdrawal right (EU/UK) and accepts no refunds.
 */
export function CheckoutConsent({ checked, onChange }: { checked: boolean; onChange: (checked: boolean) => void }) {
  return (
    <label className="flex items-start gap-2 py-2 text-xs text-muted-foreground">
      <Checkbox className="mt-0.5" checked={checked} onCheckedChange={(on) => onChange(Boolean(on))} />
      <span>
        Start the service now. I agree to the Terms of Service and the Refund Policy: the plan renews automatically until I
        cancel, cancelling stops the next renewal, payments are not refunded, and by starting now I give up my right to
        withdraw within 14 days.
      </span>
    </label>
  )
}
