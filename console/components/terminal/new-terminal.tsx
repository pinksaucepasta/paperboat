"use client"

import { PlusIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { api, idempotencyKey, useMachines, useWorkspace, type Machine } from "@/lib/api"
import { terminalKeys } from "@/lib/terminal/queries"
import { pickerOption } from "@/components/crumb-picker"
import { StatusDot } from "@/components/machine-status"
import { Button } from "@/components/ui/button"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { Command, CommandEmpty, CommandGroup, CommandInput, CommandItem, CommandList } from "@/components/ui/command"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"

/** A machine can host a terminal when it's online, you may open terminals on it, and it runs the terminal host. */
export function canHostTerminal(machine: Machine): boolean {
  const host = machine.capabilities?.terminal_host
  return (
    machine.online &&
    machine.state === "online" &&
    (machine.permissions?.includes("terminal") ?? false) &&
    !!host?.configured &&
    !!host.observed
  )
}

/** Starts a shell on a machine (in the current workspace) and returns its id. */
export function useStartShell() {
  const queryClient = useQueryClient()
  const workspace = useWorkspace()
  return useMutation({
    mutationFn: (machine: Machine) =>
      api<{ id: string }>(
        `machines/${encodeURIComponent(machine.id)}/terminal-sessions?workspace=${encodeURIComponent(workspace?.id ?? "personal")}`,
        {
          method: "POST",
          headers: { "content-type": "application/json", "idempotency-key": idempotencyKey() },
          body: JSON.stringify({}),
        },
      ),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: terminalKeys.list }),
  })
}

/** Terminals open in their own tab; `/terminals/new` starts the shell there (see StartTerminal). */
/** Opens a terminal in a new tab: on `machine` directly, or on one picked from the workspace's machines. */
export function NewTerminalButton({ machine }: { machine?: Machine }) {
  const workspace = useWorkspace()
  const start = (target: Machine) => {
    const params = new URLSearchParams({ machine: target.id, workspace: workspace?.id ?? "personal" })
    window.open(`/terminals/new?${params}`, "_blank", "noopener")
  }
  if (!machine) return <MachinePickerButton onChoose={start} />
  const ready = canHostTerminal(machine)
  return (
    <Tooltip>
      {/* A disabled button gets no pointer events, so the tooltip sits on a wrapper. */}
      <TooltipTrigger render={<span className="inline-flex" />}>
        <Button size="lg" disabled={!ready} onClick={() => start(machine)}>
          <PlusIcon data-icon="inline-start" />
          New terminal
        </Button>
      </TooltipTrigger>
      {!ready && <TooltipContent>{machine.alias} is offline or can’t host terminals</TooltipContent>}
    </Tooltip>
  )
}

function MachinePickerButton({ onChoose }: { onChoose: (machine: Machine) => void }) {
  const [open, setOpen] = useState(false)
  const machines = useMachines()
  const eligible = machines.data?.filter(canHostTerminal) ?? []
  const others = (machines.data?.length ?? 0) - eligible.length

  const choose = (machine: Machine) => {
    setOpen(false)
    onChoose(machine)
  }

  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger render={<Button size="lg" />}>
        <PlusIcon data-icon="inline-start" />
        New terminal
      </PopoverTrigger>
      <PopoverContent align="end" className="w-64 gap-0 p-0">
        <Command>
          <CommandInput placeholder="Open a terminal on…" />
          <CommandList>
            <CommandEmpty>
              {machines.isPending ? "Loading machines…" : "No machine is ready for a terminal"}
            </CommandEmpty>
            {eligible.length > 0 && (
              <CommandGroup>
                {eligible.map((machine) => (
                  <CommandItem
                    key={machine.id}
                    value={machine.alias}
                    className={pickerOption}
                    onSelect={() => choose(machine)}
                  >
                    <span className="flex min-w-0 flex-1 items-center gap-2">
                      <span className="truncate-x trim-cap">{machine.alias}</span>
                      <StatusDot online={machine.online} />
                    </span>
                    {machine.assigned_ip && (
                      <span className="font-mono text-xs text-muted-foreground">{machine.assigned_ip}</span>
                    )}
                  </CommandItem>
                ))}
              </CommandGroup>
            )}
          </CommandList>
          {others > 0 && (
            <p className="border-t border-divider px-3 py-2 text-xs text-muted-foreground">
              {others === 1 ? "1 machine is" : `${others} machines are`} offline or can’t host terminals
            </p>
          )}
        </Command>
      </PopoverContent>
    </Popover>
  )
}
