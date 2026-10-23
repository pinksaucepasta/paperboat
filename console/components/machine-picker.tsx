"use client"

import { CheckIcon, StackIcon } from "@phosphor-icons/react"
import { useMemo, useState } from "react"
import { useDebounced } from "@/hooks/use-debounced"
import { machineSearchQuery, useApiQuery, useMachine, useMachinePage, useWorkspace, type Machine } from "@/lib/api"
import { recentMachines, rememberMachine } from "@/lib/recent-machines"
import { cn } from "@/lib/utils"
import { CrumbPickerTrigger, pickerOption } from "@/components/crumb-picker"
import { StatusDot } from "@/components/machine-status"
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
  CommandSeparator,
} from "@/components/ui/command"
import { Popover, PopoverContent } from "@/components/ui/popover"
import { Skeleton } from "@/components/ui/skeleton"

type Props = {
  value: string | null
  onChange: (machineId: string | null) => void
  /** Offer "All machines"; otherwise a machine must be chosen. */
  allowAll?: boolean
}

/** Without a search the picker shows a short list; typing searches every machine on the server. */
const suggestionLimit = 6

export function MachinePicker({ value, onChange, allowAll = false }: Props) {
  const [open, setOpen] = useState(false)
  const [search, setSearch] = useState("")
  const query = useDebounced(search.trim(), 200)
  const workspace = useWorkspace()
  const page = useMachinePage()
  const { data: selected, isPending } = useMachine(value)
  const results = useApiQuery({
    ...machineSearchQuery(workspace?.id ?? "", query),
    enabled: open && Boolean(workspace) && query.length > 0,
    placeholderData: (previous) => previous,
  })

  const suggestions = useMemo(
    () =>
      page.data
        ? suggest(
            page.data.items.filter((machine) => machine.state !== "revoked"),
            value,
            workspace ? recentMachines(workspace.id) : [],
          )
        : [],
    // Recents are read when the list opens; they only change by choosing here.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [page.data, value, workspace, open],
  )

  if (value && isPending) return <Skeleton className="h-5 w-28" />

  const searching = query.length > 0
  const machines = searching ? (results.data?.items ?? []) : suggestions
  const total = page.data?.total ?? 0

  const choose = (id: string | null) => {
    setOpen(false)
    setSearch("")
    if (id && workspace) rememberMachine(workspace.id, id)
    if (id !== value) onChange(id)
  }

  return (
    <Popover
      open={open}
      onOpenChange={(next) => {
        setOpen(next)
        if (!next) setSearch("")
      }}
    >
      <CrumbPickerTrigger>
        {selected ? (
          <>
            <span className="max-w-48 truncate-x text-foreground trim-cap">{selected.alias}</span>
            <StatusDot online={selected.online} />
          </>
        ) : (
          <span className="trim-cap">{allowAll ? "All machines" : "Choose machine"}</span>
        )}
      </CrumbPickerTrigger>
      <PopoverContent align="start" className="w-64 gap-0 p-0">
        <Command shouldFilter={false} defaultValue={value ?? (allowAll ? "all" : undefined)}>
          <CommandInput
            placeholder={total > suggestionLimit ? `Search ${total} machines` : "Search machines"}
            value={search}
            onValueChange={setSearch}
          />
          <CommandList>
            <CommandEmpty>{searching && results.isFetching ? "Searching…" : "No machines found"}</CommandEmpty>
            {allowAll && !searching && (
              <>
                <CommandGroup>
                  <CommandItem value="all" className={pickerOption} onSelect={() => choose(null)}>
                    <StackIcon className="text-muted-foreground" />
                    <span className="flex-1">All machines</span>
                    <CheckIcon weight="bold" className={cn("size-3.5 text-primary", value && "hidden")} />
                  </CommandItem>
                </CommandGroup>
                <CommandSeparator className="my-0 bg-divider" />
              </>
            )}
            {machines.length > 0 && (
              <CommandGroup>
                {machines.map((machine) => (
                  <MachineOption
                    key={machine.id}
                    machine={machine}
                    current={machine.id === value}
                    onSelect={() => choose(machine.id)}
                  />
                ))}
              </CommandGroup>
            )}
          </CommandList>
          {!searching && total > machines.length && (
            <p className="border-t border-divider px-3 py-2 text-xs text-muted-foreground">
              Type to search all {total} machines
            </p>
          )}
        </Command>
      </PopoverContent>
    </Popover>
  )
}

function MachineOption({ machine, current, onSelect }: { machine: Machine; current: boolean; onSelect: () => void }) {
  return (
    <CommandItem value={machine.id} className={cn(pickerOption, current && "font-semibold")} onSelect={onSelect}>
      <span className="flex min-w-0 flex-1 items-center gap-2">
        <span className="truncate-x trim-cap">{machine.alias}</span>
        <StatusDot online={machine.online} />
      </span>
      {machine.assigned_ip && <span className="font-mono text-xs text-muted-foreground">{machine.assigned_ip}</span>}
    </CommandItem>
  )
}

/** Current first, then recently picked, then online, then the rest, capped. */
function suggest(machines: Machine[], current: string | null, recent: string[]): Machine[] {
  const rank = (machine: Machine) =>
    machine.id === current ? 0 : recent.includes(machine.id) ? 1 + recent.indexOf(machine.id) : machine.online ? 10 : 20
  return [...machines].sort((a, b) => rank(a) - rank(b) || a.alias.localeCompare(b.alias)).slice(0, suggestionLimit)
}
