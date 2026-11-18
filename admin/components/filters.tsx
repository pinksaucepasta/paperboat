"use client"

import { useEffect, useState } from "react"
import { CaretDownIcon, CheckIcon, CopyIcon, FunnelSimpleIcon, MagnifyingGlassIcon, XIcon } from "@phosphor-icons/react"
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
import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group"
import { useDebounced } from "@/hooks/use-debounced"
import { cn } from "@/lib/utils"

/** A search box that reports the typed text after a short pause, so each keystroke is not a request. */
export function SearchBox({ value, onChange, placeholder, label }: { value: string; onChange: (v: string) => void; placeholder: string; label: string }) {
  const [text, setText] = useState(value)
  const debounced = useDebounced(text, 300)
  useEffect(() => {
    if (debounced !== value) onChange(debounced.trim())
  }, [debounced, value, onChange])
  // The console's SearchField: a filled field with a search icon and a clear button.
  return (
    <InputGroup className="h-8 w-full max-w-sm border-transparent bg-surface dark:bg-surface">
      <InputGroupAddon>
        <MagnifyingGlassIcon className="size-4" />
      </InputGroupAddon>
      <InputGroupInput aria-label={label} className="md:text-sm" placeholder={placeholder} value={text} onChange={(e) => setText(e.target.value)} spellCheck={false} />
      {text && (
        <InputGroupAddon align="inline-end">
          <button type="button" aria-label="Clear search" onClick={() => setText("")} className="rounded-sm text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/30">
            <XIcon className="size-3.5" />
          </button>
        </InputGroupAddon>
      )}
    </InputGroup>
  )
}

export type Filter = { label: string; value: string; onChange: (v: string) => void; options: [string, string][] }

const control =
  "flex h-8 items-center gap-1.5 rounded-md bg-surface px-2.5 text-sm text-muted-foreground outline-none transition-colors hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/30"

function Options({ filter }: { filter: Filter }) {
  return (
    <DropdownMenuRadioGroup value={filter.value} onValueChange={(next) => filter.onChange(String(next))}>
      {filter.options.map(([value, label]) => (
        <DropdownMenuRadioItem key={value} value={value} closeOnClick>
          {label}
        </DropdownMenuRadioItem>
      ))}
    </DropdownMenuRadioGroup>
  )
}

/**
 * The console's "Filter ⌄" button and chips (console/components/machine-filters.tsx): every filter is offered in the
 * button's submenus; a set one becomes a chip whose value changes in place and whose × clears it.
 */
export function Filters({ items }: { items: Filter[] }) {
  // The button and its menu stay mounted and list every filter: unmounting an open menu (its last unset filter
  // just chosen) left its overlay blocking the page.
  return (
    <>
      {(
        <DropdownMenu>
          <DropdownMenuTrigger className={control} data-filter-button>
            <FunnelSimpleIcon className="size-4" />
            Filter
          </DropdownMenuTrigger>
          <DropdownMenuContent align="start" className="min-w-40">
            {items.map((f) => (
              <DropdownMenuSub key={f.label}>
                <DropdownMenuSubTrigger>{f.label}</DropdownMenuSubTrigger>
                <DropdownMenuSubContent className="min-w-40">
                  <Options filter={f} />
                </DropdownMenuSubContent>
              </DropdownMenuSub>
            ))}
          </DropdownMenuContent>
        </DropdownMenu>
      )}
      {items
        .filter((f) => f.value)
        .map((f) => (
          <span key={f.label} className="flex h-8 items-center rounded-md bg-surface text-sm" data-filter-chip={f.label}>
            <DropdownMenu>
              <DropdownMenuTrigger className="flex h-full items-center gap-1.5 rounded-l-md pr-1.5 pl-2.5 outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/30">
                <span className="text-muted-foreground">{f.label}</span>
                {f.options.find(([v]) => v === f.value)?.[1] ?? f.value}
                <CaretDownIcon className="size-3.5 text-muted-foreground" />
              </DropdownMenuTrigger>
              <DropdownMenuContent align="start" className="min-w-40">
                <Options filter={f} />
              </DropdownMenuContent>
            </DropdownMenu>
            <button
              type="button"
              aria-label={`Remove ${f.label} filter`}
              onClick={() => f.onChange("")}
              className="flex h-full items-center rounded-r-md px-2 text-muted-foreground outline-none hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/30"
            >
              <XIcon className="size-3.5" />
            </button>
          </span>
        ))}
    </>
  )
}

const good = new Set(["signing", "actioned", "active", "online", "ready", "succeeded", "enabled", "connected", "resolved"])
const bad = new Set(["suspended", "failed", "dead_letter", "past_due", "unpaid", "revoked", "stale", "uncertain", "disabled", "offline", "unhealthy", "draining", "blocked", "legal_block", "expired"])

/** A state as the console shows one: a dot and the word. */
export function StatusBadge({ state }: { state: string }) {
  return (
    <span className="inline-flex items-center gap-1.5 whitespace-nowrap" data-state-word={state}>
      <span className={cn("size-1.5 shrink-0 rounded-full", good.has(state) ? "bg-success" : bad.has(state) ? "bg-destructive" : "bg-muted-foreground/35")} aria-hidden />
      <span className="inline-block first-letter:uppercase">{state.replaceAll("_", " ")}</span>
    </span>
  )
}

export function Mono({ children }: { children: React.ReactNode }) {
  return <span className="font-mono text-xs">{children}</span>
}

/** An id or address that copies when clicked; the icon shows the result (the console's CopyRow). */
export function Copyable({ value }: { value: string }) {
  const [copied, setCopied] = useState(false)
  return (
    <button
      type="button"
      className="group inline-flex max-w-full items-center gap-1.5 rounded-sm text-left outline-none focus-visible:ring-2 focus-visible:ring-ring/30"
      aria-label={copied ? "Copied" : `Copy ${value}`}
      onClick={() =>
        navigator.clipboard?.writeText(value).then(
          () => {
            setCopied(true)
            setTimeout(() => setCopied(false), 1500)
          },
          () => {},
        )
      }
    >
      <span className="truncate font-mono text-xs">{value}</span>
      {copied ? <CheckIcon className="size-3.5 shrink-0 text-success" /> : <CopyIcon className="size-3.5 shrink-0 text-muted-foreground opacity-0 group-hover:opacity-100 group-focus-visible:opacity-100" />}
    </button>
  )
}
