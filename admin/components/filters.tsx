"use client"

import { useEffect, useState } from "react"
import { Input } from "@/components/ui/input"
import { Badge } from "@/components/ui/badge"
import { useDebounced } from "@/hooks/use-debounced"

/** A search box that reports the typed text after a short pause, so each keystroke is not a request. */
export function SearchBox({ value, onChange, placeholder, label }: { value: string; onChange: (v: string) => void; placeholder: string; label: string }) {
  const [text, setText] = useState(value)
  const debounced = useDebounced(text, 300)
  useEffect(() => {
    if (debounced !== value) onChange(debounced.trim())
  }, [debounced, value, onChange])
  return <Input aria-label={label} className="w-64" placeholder={placeholder} value={text} onChange={(e) => setText(e.target.value)} />
}

/** A native select: the options are a short fixed list, and it works on every phone. */
export function FilterSelect({ label, value, onChange, options }: { label: string; value: string; onChange: (v: string) => void; options: [string, string][] }) {
  return (
    <select
      aria-label={label}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      className="h-7 rounded-md border border-input bg-transparent px-2 text-xs outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
    >
      {options.map(([v, text]) => (
        <option key={v} value={v}>
          {text}
        </option>
      ))}
    </select>
  )
}

const tone: Record<string, "default" | "secondary" | "destructive" | "outline"> = {
  active: "secondary", online: "secondary", ready: "secondary", succeeded: "secondary", enabled: "secondary",
  suspended: "destructive", failed: "destructive", dead_letter: "destructive", past_due: "destructive", unpaid: "destructive", revoked: "destructive", stale: "destructive",
  uncertain: "destructive",
}

export function StatusBadge({ state }: { state: string }) {
  return <Badge variant={tone[state] ?? "outline"}>{state.replaceAll("_", " ")}</Badge>
}

export function Mono({ children }: { children: React.ReactNode }) {
  return <span className="font-mono text-xs">{children}</span>
}
