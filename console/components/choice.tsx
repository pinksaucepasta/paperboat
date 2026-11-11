"use client"

import { CaretDownIcon } from "@phosphor-icons/react"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"

/** One editable default in the settings line: its value with a caret, a radio menu on click. */
export function Choice({
  label,
  value,
  options,
  onChange,
  min = 2,
}: {
  label: string
  value: string
  options: { value: string; label: string }[]
  onChange: (value: string) => void
  /** Fewest options that make it a menu; below it, a plain label. */
  min?: number
}) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        disabled={options.length < min}
        className="inline-flex items-center gap-0.5 rounded-sm font-medium text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring/30 disabled:pointer-events-none"
      >
        {label}
        {options.length >= min && <CaretDownIcon className="size-3 text-muted-foreground" />}
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="w-max max-w-72 min-w-40">
        <DropdownMenuRadioGroup value={value} onValueChange={onChange}>
          {options.map((option) => (
            <DropdownMenuRadioItem key={option.value} value={option.value} closeOnClick>
              <span className="min-w-0 truncate">{option.label}</span>
            </DropdownMenuRadioItem>
          ))}
        </DropdownMenuRadioGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
