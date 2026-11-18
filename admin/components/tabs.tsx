"use client"

import { cn } from "@/lib/utils"

/** A page's views, as the console's machine tabs: underlined text on a divider. The choice is plain page state. */
export function Tabs<T extends string>({ value, onChange, options }: { value: T; onChange: (v: T) => void; options: [T, string][] }) {
  return (
    <div className="border-b border-divider">
      <div role="tablist" className="-mb-px no-scrollbar flex gap-1 overflow-x-auto overflow-y-hidden">
        {options.map(([v, label]) => (
          <button
            key={v}
            type="button"
            role="tab"
            aria-selected={value === v}
            onClick={() => onChange(v)}
            className={cn(
              "shrink-0 border-b-2 border-transparent px-2.5 pb-2.5 text-sm text-muted-foreground rounded-t-sm outline-none transition-colors hover:text-foreground focus-visible:text-foreground focus-visible:ring-2 focus-visible:ring-ring/30",
              value === v && "border-foreground text-foreground",
            )}
          >
            {label}
          </button>
        ))}
      </div>
    </div>
  )
}
