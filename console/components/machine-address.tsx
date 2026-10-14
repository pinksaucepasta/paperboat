"use client"

import { CaretDownIcon } from "@phosphor-icons/react"
import type { Machine } from "@/lib/api"
import { useMachineHostnames } from "@/lib/machine-domains"
import { CopyRow, CopyText } from "@/components/copy-button"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"

/** Every address a machine has, each copied by clicking its row; `ip` adds the IP. */
function AddressList({ machine, hostnames, ip }: { machine: Machine; hostnames: string[]; ip?: boolean }) {
  return (
    <>
      {hostnames.map((hostname) => (
        <CopyRow key={hostname} value={hostname} className="font-mono text-code" />
      ))}
      {ip && machine.assigned_ip && <CopyRow value={machine.assigned_ip} className="font-mono text-code" />}
    </>
  )
}

/** The machines table's address cell: the default-domain address over the IP, each copying on click. */
export function MachineAddressStack({ machine }: { machine: Machine }) {
  const [hostname] = useMachineHostnames(machine.alias)
  return (
    <div className="flex min-w-0 flex-col items-start font-mono text-code">
      <CopyText value={hostname} />
      {machine.assigned_ip && <CopyText value={machine.assigned_ip} className="text-xs text-muted-foreground" />}
    </div>
  )
}

/** The machine details' Address: the default-domain address and "+N"; clicking opens every address. */
export function MachineAddressDetail({ machine }: { machine: Machine }) {
  const hostnames = useMachineHostnames(machine.alias)
  return (
    <Popover>
      <PopoverTrigger className="-ml-2.5 flex w-full items-center justify-between gap-4 rounded-md py-1.5 pr-2 pl-2.5 text-left font-mono text-code outline-none hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring/30 data-popup-open:bg-accent">
        <span className="truncate">{hostnames[0]}</span>
        {hostnames.length > 1 && (
          <span className="shrink-0 font-sans text-xs text-muted-foreground">+{hostnames.length - 1}</span>
        )}
      </PopoverTrigger>
      {/* As wide as the row that opened it, so each copy icon sits under the "+N" you clicked. */}
      <PopoverContent align="start" className="w-(--anchor-width) gap-0 p-1">
        <AddressList machine={machine} hostnames={hostnames} />
      </PopoverContent>
    </Popover>
  )
}

/** The machine page header's IP, opening every way to reach the machine. */
export function MachineAddress({ machine }: { machine: Machine }) {
  const hostnames = useMachineHostnames(machine.alias)
  if (!machine.assigned_ip) return null
  return (
    <Popover>
      <PopoverTrigger className="relative z-10 inline-flex items-center gap-1 rounded-sm font-mono text-code outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/30">
        {machine.assigned_ip}
        <CaretDownIcon className="size-3.5 shrink-0 text-muted-foreground" />
      </PopoverTrigger>
      <PopoverContent align="start" className="w-auto min-w-64 gap-0 p-1">
        <AddressList machine={machine} hostnames={hostnames} ip />
      </PopoverContent>
    </Popover>
  )
}
