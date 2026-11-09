"use client"

import { useMutation, useQueryClient } from "@tanstack/react-query"
import { ArrowCircleUpIcon, WarningCircleIcon, CircleNotchIcon } from "@phosphor-icons/react"
import { useState } from "react"
import { approveUpdate, useWorkspace, type Machine } from "@/lib/api"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { CommandBlock } from "@/components/add-machine-dialog"

/** An interrupted update the machine could not finish or undo (withdrawn version, stuck attempt, unreachable updater); the API does not say which. */
export const updateNeedsAttention = (machine: Machine) =>
  machine.update?.error_code === "recovery_required" || machine.availability?.update_health === "recovery_required"

const inProgress = new Set(["checking", "downloading", "staged", "activating"])

/**
 * Sits beside the version. A machine that has a newer release waiting for approval ("deferred")
 * offers Update; one that is updating or failed says so. Up-to-date machines show nothing.
 */
export function MachineUpdate({ machine }: { machine: Machine }) {
  // The machine list and the machine page carry each machine's last update report.
  const update = machine.update
  const [open, setOpen] = useState(false)
  const queryClient = useQueryClient()
  const workspace = useWorkspace()?.id ?? ""
  const approve = useMutation({
    mutationFn: () => approveUpdate(workspace, machine.id, update!.target_version!),
    onSuccess: () => {
      setOpen(false)
      return queryClient.invalidateQueries({ queryKey: ["machines"] })
    },
  })

  if (updateNeedsAttention(machine)) {
    return (
      <Hint label="Update needs attention">
        <WarningCircleIcon className="size-4 text-destructive" />
      </Hint>
    )
  }
  const target = update?.target_version
  if (!update || !target) return null

  if (inProgress.has(update.state)) {
    return (
      <Hint label={`Updating to ${target}`}>
        <CircleNotchIcon className="size-4 animate-spin text-muted-foreground" />
      </Hint>
    )
  }
  if (update.state === "failed" || update.state === "rolled_back") {
    return (
      <Hint label={`Update to ${target} ${update.state === "rolled_back" ? "was rolled back" : "failed"}`}>
        <WarningCircleIcon className="size-4 text-destructive" />
      </Hint>
    )
  }
  if (update.state !== "deferred") return null

  return (
    <>
      <Hint label={`Update to ${target}`} onClick={() => setOpen(true)}>
        <ArrowCircleUpIcon className="size-4 text-primary" />
      </Hint>
      <Dialog open={open} onOpenChange={(next) => !approve.isPending && setOpen(next)}>
        <DialogContent className="sm:max-w-md">
          <DialogHeader>
            <DialogTitle>Update {machine.alias}</DialogTitle>
            <DialogDescription>
              {update.current_version} → {target}. Paperboat restarts on the machine; open sessions reconnect.
            </DialogDescription>
          </DialogHeader>
          {approve.isError && <p className="text-sm text-destructive">{approve.error.message}</p>}
          <DialogFooter>
            <DialogClose render={<Button variant="outline" size="lg" disabled={approve.isPending} />}>
              Cancel
            </DialogClose>
            <Button size="lg" disabled={approve.isPending} onClick={() => approve.mutate()}>
              {approve.isPending ? "Updating…" : "Update"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}

/** On the machine page: why the update stopped and the one command that fixes it, run on that machine. */
export function UpdateAttention({ machine }: { machine: Machine }) {
  if (!updateNeedsAttention(machine)) return null
  return (
    <div className="flex flex-col gap-3 rounded-lg bg-destructive/5 p-4 dark:bg-destructive/10">
      <div className="flex flex-col gap-1">
        <h2 className="flex items-center gap-2 text-sm font-medium">
          <WarningCircleIcon className="size-4 text-destructive" />
          Update needs attention
        </h2>
        <p className="text-sm text-muted-foreground">
          The last update didn’t finish and this machine couldn’t recover on its own. Run this on the machine to
          install the current release.
        </p>
      </div>
      <CommandBlock command="pb update" />
    </div>
  )
}

function Hint({ label, onClick, children }: { label: string; onClick?: () => void; children: React.ReactNode }) {
  return (
    <Tooltip>
      <TooltipTrigger
        render={
          onClick ? (
            <button
              type="button"
              onClick={onClick}
              className="relative z-10 inline-flex rounded-full p-0.5 hover:bg-accent"
            />
          ) : (
            <span className="relative z-10 inline-flex p-0.5" />
          )
        }
        aria-label={label}
      >
        {children}
      </TooltipTrigger>
      <TooltipContent side="top">{label}</TooltipContent>
    </Tooltip>
  )
}
