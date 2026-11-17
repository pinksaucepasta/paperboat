"use client"

import { Button } from "@/components/ui/button"

/** A row of buttons choosing which table shows; the choice is plain page state. */
export function Tabs<T extends string>({ value, onChange, options }: { value: T; onChange: (v: T) => void; options: [T, string][] }) {
  return (
    <div role="tablist" className="flex flex-wrap gap-1">
      {options.map(([v, label]) => (
        <Button key={v} role="tab" aria-selected={value === v} variant={value === v ? "secondary" : "ghost"} size="sm" onClick={() => onChange(v)}>
          {label}
        </Button>
      ))}
    </div>
  )
}
