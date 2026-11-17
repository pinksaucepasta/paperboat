"use client"

import { useId } from "react"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"

/** A labelled text input for action forms. */
export function TextField({ label, value, onChange, hint, ...rest }: { label: string; value: string; onChange: (v: string) => void; hint?: string } & Omit<React.ComponentProps<typeof Input>, "onChange" | "value">) {
  const id = useId()
  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={id}>{label}</Label>
      <Input id={id} value={value} onChange={(e) => onChange(e.target.value)} {...rest} />
      {hint && <span className="text-xs text-muted-foreground">{hint}</span>}
    </div>
  )
}

/** A labelled native select for action forms. */
export function SelectField({ label, value, onChange, options, hint, disabled }: { label: string; value: string; onChange: (v: string) => void; options: [string, string][]; hint?: string; disabled?: boolean }) {
  const id = useId()
  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={id}>{label}</Label>
      <select id={id} value={value} disabled={disabled} onChange={(e) => onChange(e.target.value)} className="h-7 rounded-md border border-input bg-transparent px-2 text-xs outline-none focus-visible:ring-2 focus-visible:ring-ring/30">
        {options.map(([v, text]) => (
          <option key={v} value={v}>{text}</option>
        ))}
      </select>
      {hint && <span className="text-xs text-muted-foreground">{hint}</span>}
    </div>
  )
}
