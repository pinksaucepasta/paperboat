"use client"

import { useId } from "react"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"

/** A labelled text input for action forms. */
export function TextField({ label, value, onChange, hint, optional, ...rest }: { label: string; value: string; onChange: (v: string) => void; hint?: string; optional?: boolean } & Omit<React.ComponentProps<typeof Input>, "onChange" | "value">) {
  const id = useId()
  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex items-baseline justify-between gap-2">
        <Label htmlFor={id}>{label}</Label>
        {optional && <span className="text-xs text-muted-foreground">Optional</span>}
      </div>
      <Input id={id} value={value} onChange={(e) => onChange(e.target.value)} aria-describedby={hint ? `${id}-hint` : undefined} {...rest} />
      {hint && <span id={`${id}-hint`} className="text-xs text-muted-foreground">{hint}</span>}
    </div>
  )
}

/** A labelled native select for action forms. */
export function SelectField({ label, value, onChange, options, hint, disabled }: { label: string; value: string; onChange: (v: string) => void; options: [string, string][]; hint?: string; disabled?: boolean }) {
  const id = useId()
  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={id}>{label}</Label>
      <select id={id} aria-describedby={hint ? `${id}-hint` : undefined} value={value} disabled={disabled} onChange={(e) => onChange(e.target.value)} className="h-7 rounded-md border border-input bg-input/20 px-2 text-sm outline-none focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/30 md:text-xs/relaxed dark:bg-input/30">
        {options.map(([v, text]) => (
          <option key={v} value={v}>{text}</option>
        ))}
      </select>
      {hint && <span id={`${id}-hint`} className="text-xs text-muted-foreground">{hint}</span>}
    </div>
  )
}
