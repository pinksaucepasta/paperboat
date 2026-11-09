"use client"

import { useQueryClient } from "@tanstack/react-query"
import { toast } from "sonner"
import { removeMachine, type Machine } from "@/lib/api"
import { ConfirmAction } from "@/components/list-toolbar"

/**
 * Removing a machine cannot be undone, so it asks for the machine's name, the way GitHub and
 * Vercel confirm deleting a repository or project.
 */
export function RemoveMachineDialog({
  machine,
  onClose,
  onRemoved,
}: {
  machine: Machine
  onClose: () => void
  onRemoved?: () => void
}) {
  const queryClient = useQueryClient()
  return (
    <ConfirmAction
      open
      onOpenChange={(open) => !open && onClose()}
      title={`Remove ${machine.alias}?`}
      description="It loses access right away and its open sessions end. To use it again, add it as a new machine."
      action="Remove machine"
      pending="Removing…"
      confirmName={machine.alias}
      mutation={{
        mutationFn: () => removeMachine(machine.id),
        onSuccess: async () => {
          toast.success(`${machine.alias} removed`)
          await queryClient.invalidateQueries({ queryKey: ["machines"] })
          onRemoved?.()
        },
      }}
    />
  )
}
