"use client"

import { CaretUpDownIcon } from "@phosphor-icons/react"
import { PopoverTrigger } from "@/components/ui/popover"

/**
 * The trigger every breadcrumb picker uses (workspace, machine): the crumb itself, which
 * brightens on hover and while open, with no box. Keeping one trigger keeps the pickers alike.
 */
export function CrumbPickerTrigger({ children }: { children: React.ReactNode }) {
  return (
    <PopoverTrigger className="flex h-7 items-center gap-2 rounded-sm text-sm font-medium text-muted-foreground transition-colors outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/30 data-popup-open:text-foreground">
      {children}
      <CaretUpDownIcon className="size-3.5" />
    </PopoverTrigger>
  )
}

/** Picker rows mark the current item by weight; cmdk's own check follows the highlight instead. */
export const pickerOption = "[&_[data-slot=command-check]]:hidden"
