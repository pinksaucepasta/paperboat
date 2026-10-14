"use client"

import { useMutation, useQueryClient } from "@tanstack/react-query"
import { ArrowCircleUpIcon, WarningCircleIcon, CircleNotchIcon } from "@phosphor-icons/react"
import { useState } from "react"
import { approveUpdate, updateSummaryQuery, useUpdate, type Machine } from "@/lib/api"
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

const inProgress = new Set(["checking", "downloading", "staged", "activating"])

/**
 * Sits beside the version. A machine that has a newer release waiting for approval ("deferred")
 * offers Update; one that is updating or failed says so. Up-to-date machines show nothing.
 */
export function MachineUpdate({ machine }: { machine: Machine }) {
  const update = useUpdate(machine.id)
  const [open, setOpen] = useState(false)
  const queryClient = useQueryClient()
  const approve = useMutation({
    mutationFn: () => approveUpdate(machine.id, update!.target_version!),
    onSuccess: () => {
      setOpen(false)
      return queryClient.invalidateQueries({ queryKey: updateSummaryQuery.queryKey })
    },
  })

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
