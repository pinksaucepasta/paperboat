"use client"

import { DotsThreeIcon, type Icon } from "@phosphor-icons/react"
import Link from "next/link"
import { Button } from "@/components/ui/button"
import {
  ContextMenu,
  ContextMenuCheckboxItem,
  ContextMenuContent,
  ContextMenuItem,
  ContextMenuSeparator,
  ContextMenuTrigger,
} from "@/components/ui/context-menu"
import {
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"

/** One menu, shown two ways for every table row: the row's "…" button and a right-click anywhere on it. */
export type Entry =
  | {
      kind: "item"
      label: string
      icon: Icon
      href?: string
      onSelect?: () => void
      destructive?: boolean
      disabled?: boolean
    }
  | {
      kind: "check"
      label: string
      icon: Icon
      checked: boolean
      disabled: boolean
      onChange: (checked: boolean) => void
    }
  | { kind: "separator" }

const dropdownParts = {
  Item: DropdownMenuItem,
  CheckboxItem: DropdownMenuCheckboxItem,
  Separator: DropdownMenuSeparator,
}
const contextParts = {
  Item: ContextMenuItem,
  CheckboxItem: ContextMenuCheckboxItem,
  Separator: ContextMenuSeparator,
}

function MenuEntries({ entries, parts }: { entries: Entry[]; parts: typeof dropdownParts | typeof contextParts }) {
  const { Item, CheckboxItem, Separator } = parts
  return entries.map((entry, index) => {
    if (entry.kind === "separator") return <Separator key={index} />
    const { icon: EntryIcon, label } = entry
    if (entry.kind === "check") {
      return (
        <CheckboxItem key={label} checked={entry.checked} disabled={entry.disabled} onCheckedChange={entry.onChange}>
          <EntryIcon /> {label}
        </CheckboxItem>
      )
    }
    return (
      <Item
        key={label}
        variant={entry.destructive ? "destructive" : "default"}
        disabled={entry.disabled}
        onClick={entry.onSelect}
        render={entry.href ? <Link href={entry.href} /> : undefined}
      >
        <EntryIcon /> {label}
      </Item>
    )
  })
}

/**
 * Wraps a table row: right-clicking anywhere on it opens its menu, and the row receives the "…"
 * button that opens the same menu. Machines and terminals use it, so their rows behave alike.
 */
export function RowMenus({
  label,
  entries,
  dialogs,
  row,
}: {
  label: string
  entries: Entry[]
  dialogs?: React.ReactNode
  row: (actions: React.ReactNode) => React.ReactElement
}) {
  const button = (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={
          <Button
            variant="ghost"
            size="icon-sm"
            aria-label={label}
            className="relative z-10 text-muted-foreground hover:text-foreground"
          />
        }
      >
        <DotsThreeIcon weight="bold" className="size-4" />
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-56">
        <MenuEntries entries={entries} parts={dropdownParts} />
      </DropdownMenuContent>
    </DropdownMenu>
  )
  return (
    <>
      <ContextMenu>
        <ContextMenuTrigger render={row(button)} />
        <ContextMenuContent className="w-56">
          <MenuEntries entries={entries} parts={contextParts} />
        </ContextMenuContent>
      </ContextMenu>
      {dialogs}
    </>
  )
}
