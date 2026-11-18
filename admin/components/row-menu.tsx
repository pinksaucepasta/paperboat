"use client"

import { DotsThreeIcon } from "@phosphor-icons/react"
import { Fragment, useState } from "react"
import { Button } from "@/components/ui/button"
import { DropdownMenu, DropdownMenuContent, DropdownMenuItem, DropdownMenuSeparator, DropdownMenuTrigger } from "@/components/ui/dropdown-menu"

export type DialogControl = { open: boolean; onOpenChange: (open: boolean) => void }
export type RowAction = { key: string; label: string; destructive?: boolean; dialog: (control: DialogControl) => React.ReactNode }

/**
 * A row's "…" menu (the console's row menu): each item opens its confirmation, which lives outside the menu so it
 * stays open after the menu closes. `data-actions` lists the offered keys, for checks. Destructive items come last.
 */
export function RowMenu({ label, actions, ...rest }: { label: string; actions: RowAction[] } & Record<`data-${string}`, string>) {
  const [shown, setShown] = useState<string>()
  if (actions.length === 0) return null
  const plain = actions.filter((a) => !a.destructive)
  const risky = actions.filter((a) => a.destructive)
  const item = (a: RowAction) => (
    <DropdownMenuItem key={a.key} variant={a.destructive ? "destructive" : "default"} onClick={() => setShown(a.key)} data-row-action={a.key}>
      {a.label}…
    </DropdownMenuItem>
  )
  return (
    <div className="flex justify-end" data-actions={actions.map((a) => a.key).join(" ")} {...rest}>
      <DropdownMenu>
        <DropdownMenuTrigger render={<Button variant="ghost" size="icon-sm" aria-label={`Actions for ${label}`} data-row-menu />}>
          <DotsThreeIcon weight="bold" />
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" className="min-w-44">
          {plain.map(item)}
          {plain.length > 0 && risky.length > 0 && <DropdownMenuSeparator />}
          {risky.map(item)}
        </DropdownMenuContent>
      </DropdownMenu>
      {/* Only the open confirmation is mounted, so each opening starts from current data (DESIGN.md: Dialogs). */}
      {actions
        .filter((a) => a.key === shown)
        .map((a) => (
          <Fragment key={a.key}>{a.dialog({ open: true, onOpenChange: (open) => setShown(open ? a.key : undefined) })}</Fragment>
        ))}
    </div>
  )
}
