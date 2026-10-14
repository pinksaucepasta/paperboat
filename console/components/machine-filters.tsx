"use client"

import { CaretDownIcon, FunnelSimpleIcon, XIcon } from "@phosphor-icons/react"
import { filterKeys, withFilter, type FilterKey, type Filters } from "@/lib/machine-filter"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSub,
  DropdownMenuSubContent,
  DropdownMenuSubTrigger,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"

const control =
  "flex h-8 items-center gap-1.5 rounded-md bg-surface px-2.5 text-sm text-muted-foreground outline-none transition-colors hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/30"

function Options({ name, value, onChange }: { name: FilterKey; value?: string; onChange: (value: string) => void }) {
  return (
    <DropdownMenuRadioGroup value={value ?? ""} onValueChange={(next) => onChange(String(next))}>
      {Object.entries(filterKeys[name].options).map(([option, label]) => (
        <DropdownMenuRadioItem key={option} value={option}>
          {label}
        </DropdownMenuRadioItem>
      ))}
    </DropdownMenuRadioGroup>
  )
}

/** "Filter ⌄": each property opens its values; picking one writes its token into the search. */
export function FilterButton({
  query,
  keys,
  filters,
  onQuery,
}: {
  query: string
  keys: FilterKey[]
  filters: Filters
  onQuery: (query: string) => void
}) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger className={control}>
        <FunnelSimpleIcon className="size-4" />
        Filter
      </DropdownMenuTrigger>
      <DropdownMenuContent align="start" className="min-w-40">
        {keys.map((name) => (
          <DropdownMenuSub key={name}>
            <DropdownMenuSubTrigger>{filterKeys[name].label}</DropdownMenuSubTrigger>
            <DropdownMenuSubContent className="min-w-40">
              <Options
                name={name}
                value={filters[name]}
                onChange={(value) => onQuery(withFilter(query, name, value))}
              />
            </DropdownMenuSubContent>
          </DropdownMenuSub>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

/** One applied filter: its value changes in place, × removes its token. */
export function FilterChip({
  name,
  value,
  query,
  onQuery,
}: {
  name: FilterKey
  value: string
  query: string
  onQuery: (query: string) => void
}) {
  const spec = filterKeys[name]
  const label = spec.options[value as keyof typeof spec.options]
  return (
    <span className="flex h-8 items-center rounded-md bg-surface text-sm">
      <DropdownMenu>
        <DropdownMenuTrigger className="flex h-full items-center gap-1.5 rounded-l-md pr-1.5 pl-2.5 outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/30">
          <span className="text-muted-foreground">{spec.label}</span>
          {label}
          <CaretDownIcon className="size-3.5 text-muted-foreground" />
        </DropdownMenuTrigger>
        <DropdownMenuContent align="start" className="min-w-40">
          <Options name={name} value={value} onChange={(next) => onQuery(withFilter(query, name, next))} />
        </DropdownMenuContent>
      </DropdownMenu>
      <button
        type="button"
        aria-label={`Remove ${spec.label} filter`}
        onClick={() => onQuery(withFilter(query, name))}
        className="flex h-full items-center rounded-r-md px-2 text-muted-foreground outline-none hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/30"
      >
        <XIcon className="size-3.5" />
      </button>
    </span>
  )
}
