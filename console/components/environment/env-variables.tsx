"use client"

import { TrashIcon } from "@phosphor-icons/react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { toast } from "sonner"
import { DeliveryPending, deleteValue, listNames, setValue, type EnvScope } from "@/lib/envvault/vault"
import { ConfirmAction } from "@/components/list-toolbar"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldError, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"

const namePattern = /^[A-Za-z_][A-Za-z0-9_]*$/

/** Names with Set and Remove, and a form to add one. Values go in, never out. */
export function EnvVariables({ scope }: { scope: EnvScope }) {
  const client = useQueryClient()
  const key = ["env-names", scope]
  const names = useQuery({ queryKey: key, queryFn: () => listNames(scope), staleTime: 0 })
  const [editing, setEditing] = useState<string | null>(null)
  const [removing, setRemoving] = useState<string | null>(null)
  const refresh = () => client.invalidateQueries({ queryKey: key })
  // The change is saved even when some machine still needs its key; say so instead of failing.
  const saved = (error: Error) => {
    if (error instanceof DeliveryPending) {
      toast.message(error.message)
      return refresh()
    }
    toast.error(error.message)
  }
  return (
    <div className="flex flex-col">
      <div className="flex min-h-12 items-center border-t border-divider py-2">
        <Button variant="secondary" size="lg" onClick={() => setEditing("")}>
          Add variable
        </Button>
      </div>
      {names.isPending && <Skeleton className="h-12 w-full" />}
      {names.isError && <p className="py-4 text-sm text-destructive">{names.error.message}</p>}
      {names.data?.length === 0 && (
        <p className="border-t border-divider py-4 text-sm text-muted-foreground">No variables yet.</p>
      )}
      {names.data?.map((name) => (
        <div key={name} className="flex min-h-12 items-center gap-3 border-t border-divider py-2 text-sm">
          <code className="min-w-0 flex-1 truncate font-mono text-xs">{name}</code>
          <span className="text-xs text-muted-foreground">Value hidden</span>
          <Button variant="ghost" size="lg" onClick={() => setEditing(name)}>
            Set value
          </Button>
          <Button variant="ghost" size="icon" aria-label={`Remove ${name}`} onClick={() => setRemoving(name)}>
            <TrashIcon />
          </Button>
        </div>
      ))}
      {editing !== null && (
        <SetDialog
          scope={scope}
          name={editing}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null)
            refresh()
          }}
          onError={saved}
        />
      )}
      <ConfirmAction
        open={removing !== null}
        onOpenChange={(open) => !open && setRemoving(null)}
        title={`Remove ${removing ?? ""}?`}
        description="Machines stop receiving it the next time a process starts there."
        action="Remove"
        pending="Removing…"
        mutation={{
          mutationFn: () => deleteValue(scope, removing ?? ""),
          onSuccess: refresh,
          onError: saved,
        }}
      />
    </div>
  )
}

function SetDialog({
  scope,
  name: initial,
  onClose,
  onSaved,
  onError,
}: {
  scope: EnvScope
  name: string
  onClose: () => void
  onSaved: () => void
  onError: (error: Error) => void
}) {
  const [name, setName] = useState(initial)
  const [value, setValue_] = useState("")
  const save = useMutation({
    mutationFn: (input: { name: string; value: string }) => setValue(scope, input.name, input.value),
    onSuccess: onSaved,
    onError,
  })
  const valid = namePattern.test(name) && name.length <= 128
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{initial ? `Set ${initial}` : "Add variable"}</DialogTitle>
        </DialogHeader>
        <form
          className="flex flex-col gap-4"
          onSubmit={(event) => {
            event.preventDefault()
            const secret = value
            setValue_("")
            if (valid) save.mutate({ name, value: secret })
          }}
        >
          {!initial && (
            <Field className="gap-1.5">
              <FieldLabel htmlFor="env-name">Name</FieldLabel>
              <Input
                id="env-name"
                value={name}
                onChange={(event) => setName(event.target.value)}
                autoComplete="off"
                spellCheck={false}
                autoFocus
              />
              <p className="text-xs text-muted-foreground">
                Letters, digits and underscores, not starting with a digit.
              </p>
            </Field>
          )}
          <Field className="gap-1.5">
            <FieldLabel htmlFor="env-value">Value</FieldLabel>
            <Input
              id="env-value"
              type="password"
              name="env-value"
              autoComplete="off"
              value={value}
              onChange={(event) => setValue_(event.target.value)}
              autoFocus={Boolean(initial)}
            />
            <p className="text-xs text-muted-foreground">Encrypted here. It is never shown again.</p>
            {save.isError && !(save.error instanceof DeliveryPending) && <FieldError>{save.error.message}</FieldError>}
          </Field>
          <DialogFooter>
            <Button type="button" variant="ghost" size="lg" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" size="lg" disabled={!valid || save.isPending}>
              {save.isPending ? "Saving…" : "Save"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
