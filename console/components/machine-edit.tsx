"use client"

import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { toast } from "sonner"
import { updateMachine, type Machine } from "@/lib/api"
import { nameProblem, normalizeName } from "@/lib/enrollment"
import { useMachineHostnames } from "@/lib/machine-domains"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldError, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"

/** Name and description editing shared by the in-place title and the Edit dialog. */
export function useMachineEdit(machine: Machine, onSaved: () => void) {
  const queryClient = useQueryClient()
  const [name, setNameRaw] = useState(machine.alias)
  const [description, setDescriptionRaw] = useState(machine.description ?? "")
  const finalName = name.replace(/-+$/, "")
  const problem = finalName ? nameProblem(finalName) : "A machine needs a name"
  const changed = finalName !== machine.alias || description.trim() !== (machine.description ?? "")
  const save = useMutation({
    mutationFn: () => updateMachine(machine.id, finalName, description.trim()),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["machines"] })
      toast.success("Machine updated")
      onSaved()
    },
  })
  return {
    name,
    setName: (value: string) => setNameRaw(normalizeName(value)),
    description,
    setDescription: (value: string) => setDescriptionRaw(value.replace(/\p{Cc}/gu, "")),
    finalName,
    problem,
    changed,
    save,
    submit: () => {
      if (!changed) return onSaved()
      if (!problem) save.mutate()
    },
  }
}

/** The Edit dialog, built to DESIGN.md's dialog rules. */
export function EditMachineDialog({ machine, onClose }: { machine: Machine; onClose: () => void }) {
  const edit = useMachineEdit(machine, onClose)
  const [hostname] = useMachineHostnames(edit.finalName)
  return (
    <Dialog open onOpenChange={(next) => !next && !edit.save.isPending && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Edit machine</DialogTitle>
        </DialogHeader>
        <form
          className="flex flex-col gap-4"
          onSubmit={(event) => {
            event.preventDefault()
            edit.submit()
          }}
        >
          <Field data-invalid={Boolean(edit.problem)} className="gap-1.5">
            <FieldLabel htmlFor="edit-name">Name</FieldLabel>
            <Input
              id="edit-name"
              value={edit.name}
              onChange={(event) => edit.setName(event.target.value)}
              aria-invalid={Boolean(edit.problem)}
              autoComplete="off"
              spellCheck={false}
            />
            {edit.problem ? (
              <FieldError>{edit.problem}</FieldError>
            ) : (
              <p className="text-xs text-muted-foreground">
                Reachable as <span className="font-mono">{hostname}</span>
              </p>
            )}
          </Field>
          <Field className="gap-1.5">
            <FieldLabel htmlFor="edit-description" className="flex items-baseline justify-between">
              Description
              <span className="text-xs font-normal text-muted-foreground">Optional</span>
            </FieldLabel>
            <Input
              id="edit-description"
              placeholder="What this machine is for"
              maxLength={160}
              value={edit.description}
              onChange={(event) => edit.setDescription(event.target.value)}
              autoComplete="off"
            />
          </Field>
          {edit.save.isError && <p className="text-sm text-destructive">{edit.save.error.message}</p>}
          <DialogFooter>
            <Button type="button" variant="outline" size="lg" onClick={onClose} disabled={edit.save.isPending}>
              Cancel
            </Button>
            <Button type="submit" size="lg" disabled={Boolean(edit.problem) || edit.save.isPending}>
              {edit.save.isPending ? "Saving…" : "Save"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
