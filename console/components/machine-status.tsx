"use client"

import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { cn } from "@/lib/utils"
import type { Machine } from "@/lib/api"
import { useTimeFormat } from "@/lib/use-time-format"

export function StatusDot({ online, className }: { online: boolean; className?: string }) {
  return (
    <span
      aria-hidden
      className={cn(
        "inline-block size-2 shrink-0 rounded-full",
        online ? "bg-success" : "bg-muted-foreground/35",
        className,
      )}
    />
  )
}

/**
 * What a person needs to know about reachability, in the words Tailscale users expect. "Last seen"
 * needs the clock, so it waits for hydration (until then: Offline).
 */
function useMachineStatusLabel(machine: Machine): string {
  const time = useTimeFormat()
  if (machine.state === "revoked") return "Revoked"
  if (machine.state === "pending") return "Pending approval"
  if (machine.online) return "Connected"
  return machine.last_seen_at && time.now ? `Last seen ${time.relative(machine.last_seen_at)}` : "Offline"
}

export function MachineStatus({ machine }: { machine: Machine }) {
  const label = useMachineStatusLabel(machine)
  return (
    <span className="inline-flex items-center gap-2 text-muted-foreground">
      <StatusDot online={machine.online} />
      <span className={cn("trim-cap", machine.online && "text-foreground")}>{label}</span>
    </span>
  )
}

export function machineVersion(machine: Machine): string | undefined {
  const versions = machine.runtime_versions ?? {}
  return versions.paperboatd ?? versions.pb ?? Object.values(versions)[0]
}

/** The dot beside a machine name; hovering it says when the machine was last seen. */
export function MachineDot({ machine, className }: { machine: Machine; className?: string }) {
  const label = useMachineStatusLabel(machine)
  const time = useTimeFormat()
  return (
    <Tooltip>
      <TooltipTrigger
        render={<span className={cn("relative z-10 -m-1.5 inline-flex p-1.5", className)} />}
        aria-label={label}
      >
        <StatusDot online={machine.online} />
      </TooltipTrigger>
      <TooltipContent side="top" className="flex-col items-start gap-0.5">
        <span>{label}</span>
        {!machine.online && machine.last_seen_at && (
          <span className="opacity-70">{time.dateTime(machine.last_seen_at)}</span>
        )}
      </TooltipContent>
    </Tooltip>
  )
}
