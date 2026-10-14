"use client"

import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { toast } from "sonner"
import { removeMachine, type Machine } from "@/lib/api"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"

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
  const [typed, setTyped] = useState("")
  const confirmed = typed.trim() === machine.alias
  const remove = useMutation({
    mutationFn: () => removeMachine(machine.id),
    onSuccess: async () => {
      toast.success(`${machine.alias} removed`)
      await queryClient.invalidateQueries({ queryKey: ["machines"] })
      onClose()
      onRemoved?.()
    },
  })

  return (
    <AlertDialog open onOpenChange={(next) => !next && !remove.isPending && onClose()}>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Remove {machine.alias}?</AlertDialogTitle>
          <AlertDialogDescription>
            It loses access right away and its open sessions end. To use it again, add it as a new machine.
          </AlertDialogDescription>
        </AlertDialogHeader>
        <form
          onSubmit={(event) => {
            event.preventDefault()
            if (confirmed) remove.mutate()
          }}
        >
          <Field className="gap-1.5">
            <FieldLabel htmlFor="remove-confirm" className="font-normal text-muted-foreground">
              Type <span className="font-mono font-medium text-foreground">{machine.alias}</span> to confirm
            </FieldLabel>
            <Input
              id="remove-confirm"
              autoFocus
              value={typed}
              onChange={(event) => setTyped(event.target.value)}
              autoComplete="off"
              spellCheck={false}
            />
          </Field>
        </form>
        {remove.isError && <p className="text-sm text-destructive">{remove.error.message}</p>}
        <AlertDialogFooter>
          <AlertDialogCancel disabled={remove.isPending}>Cancel</AlertDialogCancel>
          <AlertDialogAction
            variant="destructive"
            disabled={!confirmed || remove.isPending}
            onClick={(event) => {
              // Keep the dialog open until the server confirms; Enter in the field submits too.
              event.preventDefault()
              if (confirmed) remove.mutate()
            }}
          >
            {remove.isPending ? "Removing…" : "Remove machine"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
