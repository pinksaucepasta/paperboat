"use client"

import { CaretUpDownIcon } from "@phosphor-icons/react"
import { useState } from "react"
import { useMachines, type Machine } from "@/lib/api"
import { cn } from "@/lib/utils"
import { pickerOption } from "@/components/crumb-picker"
import { StatusDot } from "@/components/machine-status"
import { Button } from "@/components/ui/button"
import { Command, CommandEmpty, CommandGroup, CommandInput, CommandItem, CommandList } from "@/components/ui/command"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"

/** A machine can run previews when it's online and its Paperboat service offers them. */
export function canPreview(machine: Machine) {
  const launch = machine.capabilities?.preview_launch
  return machine.online && Boolean(launch?.configured && launch.observed)
}

/**
 * The machine field in New preview and New tunnel. Machines that can't take part (`usable`) stay
 * listed but dimmed, with why in their row, so a missing machine never looks like a bug.
 */
export function MachineChoice({
  value,
  onChange,
  usable,
}: {
  value?: Machine
  onChange: (machine: Machine) => void
  usable: (machine: Machine) => boolean
}) {
  const [open, setOpen] = useState(false)
  const machines = useMachines()
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger
        render={
          <Button
            type="button"
            variant="outline"
            className="w-full min-w-0 justify-between font-normal"
            disabled={machines.isPending}
          />
        }
      >
        <span className="flex min-w-0 items-center gap-2">
          <span className="truncate">{value?.alias ?? (machines.isPending ? "Loading…" : "Choose a machine")}</span>
          {value && <StatusDot online={value.online} />}
        </span>
        <CaretUpDownIcon className="text-muted-foreground" />
      </PopoverTrigger>
      <PopoverContent align="start" className="w-(--anchor-width) min-w-56 gap-0 p-0">
        <Command>
          <CommandInput placeholder="Search machines" />
          <CommandList>
            <CommandEmpty>No machines</CommandEmpty>
            <CommandGroup>
              {(machines.data ?? []).map((machine) => {
                const ok = usable(machine)
                return (
                  <CommandItem
                    key={machine.id}
                    value={machine.alias}
                    disabled={!ok}
                    className={cn(pickerOption, !ok && "text-muted-foreground")}
                    onSelect={() => {
                      onChange(machine)
                      setOpen(false)
                    }}
                  >
                    <span className="flex min-w-0 flex-1 items-center gap-2">
                      <span className="truncate-x trim-cap">{machine.alias}</span>
                      <StatusDot online={machine.online} />
                    </span>
                    {!ok && <span className="text-xs">{machine.online ? "Not supported" : "Offline"}</span>}
                  </CommandItem>
                )
              })}
            </CommandGroup>
          </CommandList>
        </Command>
      </PopoverContent>
    </Popover>
  )
}

/** Digits only, 1–65535; `value` is "" until it is a usable port. */
export function usePort(initial = "3000") {
  const [text, setText] = useState(initial)
  const valid = /^\d{1,5}$/.test(text) && Number(text) > 0 && Number(text) < 65536
  return { text, set: (next: string) => setText(next.replace(/\D/g, "").slice(0, 5)), value: valid ? Number(text) : 0 }
}
