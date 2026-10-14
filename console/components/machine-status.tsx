import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { cn } from "@/lib/utils"
import type { Machine } from "@/lib/api"
import { formatDateTime, formatRelative } from "@/lib/format"

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

/** What a person needs to know about reachability, in the words Tailscale users expect. */
export function machineStatusLabel(machine: Machine): string {
  if (machine.state === "revoked") return "Revoked"
  if (machine.state === "pending") return "Pending approval"
  if (machine.online) return "Connected"
  return machine.last_seen_at ? `Last seen ${formatRelative(machine.last_seen_at)}` : "Offline"
}

export function MachineStatus({ machine }: { machine: Machine }) {
  return (
    <span className="inline-flex items-center gap-2 text-muted-foreground">
      <StatusDot online={machine.online} />
      <span className={cn("trim-cap", machine.online && "text-foreground")}>{machineStatusLabel(machine)}</span>
    </span>
  )
}

export function machineVersion(machine: Machine): string | undefined {
  const versions = machine.runtime_versions ?? {}
  return versions.paperboatd ?? versions.pb ?? Object.values(versions)[0]
}

/** The dot beside a machine name; hovering it says when the machine was last seen. */
export function MachineDot({ machine, className }: { machine: Machine; className?: string }) {
  return (
    <Tooltip>
      <TooltipTrigger
        render={<span className={cn("relative z-10 -m-1.5 inline-flex p-1.5", className)} />}
        aria-label={machineStatusLabel(machine)}
      >
        <StatusDot online={machine.online} />
      </TooltipTrigger>
      <TooltipContent side="top" className="flex-col items-start gap-0.5">
        <span>{machineStatusLabel(machine)}</span>
        {!machine.online && machine.last_seen_at && (
          <span className="opacity-70">{formatDateTime(machine.last_seen_at)}</span>
        )}
      </TooltipContent>
    </Tooltip>
  )
}
