"use client"

import { CaretUpDownIcon, CheckIcon, XIcon } from "@phosphor-icons/react"
import { useState } from "react"
import { useMachines } from "@/lib/api"
import { targetName } from "@/lib/token-scope"
import { cn } from "@/lib/utils"
import { StatusDot } from "@/components/machine-status"
import { Badge } from "@/components/ui/badge"
import { Command, CommandEmpty, CommandGroup, CommandInput, CommandItem, CommandList } from "@/components/ui/command"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"

const visibleChips = 3

/**
 * "Only selected machines": a searchable multi-select, tags first, then machines with their status;
 * the picks show as removable chips (a few, then "+N more"). Targets are `tag:x` and `machine:alias`.
 */
export function TokenMachines({
  tags,
  value,
  onChange,
}: {
  tags: string[]
  value: string[]
  onChange: (targets: string[]) => void
}) {
  const machines = useMachines()
  const [open, setOpen] = useState(false)
  const [all, setAll] = useState(false)
  const toggle = (target: string) =>
    onChange(value.includes(target) ? value.filter((v) => v !== target) : [...value, target])
  const shown = all ? value : value.slice(0, visibleChips)
  return (
    <div className="flex flex-col gap-2">
      <Popover open={open} onOpenChange={setOpen}>
        <PopoverTrigger
          className="flex h-9 w-full items-center justify-between gap-2 rounded-md border border-input bg-transparent px-2.5 text-left text-sm text-muted-foreground outline-none focus-visible:border-ring focus-visible:ring-2 focus-visible:ring-ring/30"
        >
          <span className="truncate">
            {value.length === 0 ? "Search tags and machines" : `${value.length} selected`}
          </span>
          <CaretUpDownIcon className="size-4 shrink-0" />
        </PopoverTrigger>
        <PopoverContent align="start" className="w-(--anchor-width) min-w-64 gap-0 p-0">
          <Command>
            <CommandInput placeholder="Search tags and machines" />
            <CommandList>
              <CommandEmpty>{machines.isPending ? "Loading…" : "Nothing found"}</CommandEmpty>
              {tags.length > 0 && (
                <CommandGroup heading="Tags">
                  {tags.map((tag) => (
                    <Option key={tag} target={tag} picked={value.includes(tag)} onToggle={toggle}>
                      <span className="truncate">{targetName(tag)}</span>
                    </Option>
                  ))}
                </CommandGroup>
              )}
              {(machines.data?.length ?? 0) > 0 && (
                <CommandGroup heading="Machines">
                  {machines.data?.map((machine) => (
                    <Option
                      key={machine.id}
                      target={`machine:${machine.alias}`}
                      picked={value.includes(`machine:${machine.alias}`)}
                      onToggle={toggle}
                    >
                      <span className="flex min-w-0 items-center gap-2">
                        <span className="truncate">{machine.alias}</span>
                        <StatusDot online={machine.online} />
                      </span>
                    </Option>
                  ))}
                </CommandGroup>
              )}
            </CommandList>
          </Command>
        </PopoverContent>
      </Popover>
      {value.length > 0 && (
        <div className="flex flex-wrap items-center gap-1.5">
          {shown.map((target) => (
            <Badge key={target} variant="outline" className="h-6 gap-1 pr-1 font-normal">
              <span className="max-w-40 truncate">{targetName(target)}</span>
              <button
                type="button"
                aria-label={`Remove ${targetName(target)}`}
                className="rounded-sm text-muted-foreground hover:text-foreground"
                onClick={() => toggle(target)}
              >
                <XIcon className="size-3" />
              </button>
            </Badge>
          ))}
          {value.length > visibleChips && (
            <button
              type="button"
              className="text-xs text-muted-foreground underline-offset-4 hover:text-foreground hover:underline"
              onClick={() => setAll(!all)}
            >
              {all ? "Show fewer" : `+${value.length - visibleChips} more`}
            </button>
          )}
        </div>
      )}
    </div>
  )
}

/** One row of the list: the target's name on the left, a check when picked. The popover stays open. */
function Option({
  target,
  picked,
  onToggle,
  children,
}: {
  target: string
  picked: boolean
  onToggle: (target: string) => void
  children: React.ReactNode
}) {
  return (
    <CommandItem
      value={target}
      className="[&_[data-slot=command-check]]:hidden"
      onSelect={() => onToggle(target)}
    >
      <span className="flex min-w-0 flex-1 items-center">{children}</span>
      <CheckIcon weight="bold" className={cn("size-3.5 text-primary", !picked && "invisible")} />
    </CommandItem>
  )
}
