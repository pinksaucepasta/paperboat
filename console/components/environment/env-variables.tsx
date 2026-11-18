"use client"

import { TrashIcon } from "@phosphor-icons/react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { toast } from "sonner"
import { DeliveryPending, deleteValue, listNames, setValue, type EnvScope } from "@/lib/envvault/vault"
import { validVariableName } from "@/lib/envvault/names"
import { namedVaultNames } from "@/lib/recipe-vaults"
import { ConfirmAction } from "@/components/list-toolbar"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldError, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { ErrorMessage, errorToastOf } from "@/components/error-reference"

/** Where the variables live: the account's or a team's own records, or one named vault. */
export type VariableStore = {
  key: unknown[]
  list: (signal?: AbortSignal) => Promise<string[]>
  set: (name: string, value: string) => Promise<unknown>
  remove: (name: string) => Promise<unknown>
}

export const scopeStore = (scope: EnvScope): VariableStore => ({
  key: ["env-names", scope],
  list: (signal) => listNames(scope, signal),
  set: (name, value) => setValue(scope, name, value),
  remove: (name) => deleteValue(scope, name),
})

export const vaultStore = (workspace: string, vault: string): VariableStore => ({
  key: ["env-names", "vault", workspace, vault],
  list: () => namedVaultNames(workspace, vault),
  set: (name, value) => namedVaultNames(workspace, vault, { variable: name, value }),
  remove: (name) => namedVaultNames(workspace, vault, { variable: name, value: null }),
})

/**
 * Names with Set and Remove, and a form to add one. Values go in, never out. With `adding` the
 * Add variable button lives elsewhere (the page header) and opens the form through it.
 */
export function EnvVariables({
  store,
  adding,
  onAddingChange,
}: {
  store: VariableStore
  adding?: boolean
  onAddingChange?: (adding: boolean) => void
}) {
  const client = useQueryClient()
  const key = store.key
  const names = useQuery({ queryKey: key, queryFn: ({ signal }) => store.list(signal), staleTime: 0 })
  const [ownEditing, setOwnEditing] = useState<string | null>(null)
  // A header's Add variable opens the same form with an empty name.
  const editing = adding ? "" : ownEditing
  const setEditing = (name: string | null) => {
    if (name === null) onAddingChange?.(false)
    setOwnEditing(name)
  }
  const [removing, setRemoving] = useState<string | null>(null)
  const refresh = () => client.invalidateQueries({ queryKey: key })
  // The change is saved even when some machine still needs its key; say so instead of failing.
  const saved = (error: Error) => {
    if (error instanceof DeliveryPending) {
      toast.message(error.message)
      return refresh()
    }
    errorToastOf(error)
  }
  return (
    <div className="flex flex-col">
      {adding === undefined && (
        <div className="flex min-h-12 items-center border-b border-divider py-2">
          <Button variant="secondary" size="lg" onClick={() => setEditing("")}>
            Add variable
          </Button>
        </div>
      )}
      {/* Rows as they load: the name a placeholder, the rest of the row as it is. */}
      {names.isPending &&
        Array.from({ length: 3 }, (_, index) => (
          <div key={index} className="flex min-h-12 items-center gap-3 border-b border-divider py-2 text-sm">
            <span className="min-w-0 flex-1">
              <Skeleton className="h-3 w-40 max-w-full" />
            </span>
            <span className="text-xs text-muted-foreground">Value hidden</span>
            <Button variant="ghost" size="lg" tabIndex={-1} aria-hidden className="pointer-events-none">
              Set value
            </Button>
            <Button variant="ghost" size="icon" tabIndex={-1} aria-hidden className="pointer-events-none">
              <TrashIcon />
            </Button>
          </div>
        ))}
      {names.isError && (
        <p className="py-4 text-sm text-destructive">
          <ErrorMessage error={names.error} />
        </p>
      )}
      {names.data?.length === 0 && (
        <p className="border-b border-divider py-4 text-sm text-muted-foreground">No variables yet.</p>
      )}
      {names.data?.map((name) => (
        <div key={name} className="flex min-h-12 items-center gap-3 border-b border-divider py-2 text-sm">
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
          store={store}
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
        description="Machines get the change in new terminals and commands."
        action="Remove"
        pending="Removing…"
        mutation={{
          mutationFn: () => store.remove(removing ?? ""),
          onSuccess: refresh,
          onError: saved,
        }}
      />
    </div>
  )
}

function SetDialog({
  store,
  name: initial,
  onClose,
  onSaved,
  onError,
}: {
  store: VariableStore
  name: string
  onClose: () => void
  onSaved: () => void
  onError: (error: Error) => void
}) {
  const [name, setName] = useState(initial)
  const [value, setValue_] = useState("")
  const save = useMutation({
    mutationFn: (input: { name: string; value: string }) => store.set(input.name, input.value),
    onSuccess: onSaved,
    onError,
  })
  const valid = validVariableName(name)
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
              {name && !valid ? (
                <FieldError>
                  Not allowed. LD_, DYLD_ and PAPERBOAT_ names, NODE_OPTIONS, PYTHONPATH, PYTHONHOME and GOTRACEBACK are
                  reserved.
                </FieldError>
              ) : (
                <p className="text-xs text-muted-foreground">
                  Letters, digits and underscores, not starting with a digit. Up to 128.
                </p>
              )}
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
            <p className="text-xs text-muted-foreground">You won’t see the value again.</p>
            {save.isError && !(save.error instanceof DeliveryPending) && (
              <FieldError>
                <ErrorMessage error={save.error} />
              </FieldError>
            )}
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
