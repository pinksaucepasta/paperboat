"use client"

import { PlusIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { useApiQuery, type Workspace } from "@/lib/api"
import { createNamedVault, deleteNamedVault, listNamedVaults, namedVaultsKey } from "@/lib/recipe-vaults"
import { nameHint, validName } from "@/lib/recipes"
import { ConfirmAction } from "@/components/list-toolbar"
import { DangerZone } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldError, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { ErrorMessage } from "@/components/error-reference"

// "-" stands for the default vault: a vault name never starts with it (validName).
const defaultValue = "-"

const useNamedVaults = (workspace: Workspace) =>
  useApiQuery({
    queryKey: namedVaultsKey(workspace.id),
    queryFn: ({ signal }) => listNamedVaults(workspace.id, signal),
  })

/** Environment page: the default variables, then each named vault of the workspace, and New vault. */
export function EnvVaultSwitcher({
  workspace,
  selected,
  onSelect,
}: {
  workspace: Workspace
  selected: string | null
  onSelect: (vault: string | null) => void
}) {
  const vaults = useNamedVaults(workspace)
  const [creating, setCreating] = useState(false)
  return (
    <div className="flex flex-wrap items-center gap-2 pb-3">
      {vaults.isPending && <Skeleton className="h-9 w-48" />}
      {vaults.isError && (
        <span className="flex items-center gap-3 text-sm text-destructive">
          <ErrorMessage error={vaults.error} />
          <Button variant="outline" onClick={() => vaults.refetch()}>
            Retry
          </Button>
        </span>
      )}
      {vaults.data && (
        <ToggleGroup
          aria-label="Vault"
          value={[selected ?? defaultValue]}
          onValueChange={(next) => next[0] && onSelect(next[0] === defaultValue ? null : next[0])}
          variant="outline"
          spacing={2}
          className="flex-wrap"
        >
          <ToggleGroupItem value={defaultValue}>Default</ToggleGroupItem>
          {vaults.data.map((vault) => (
            <ToggleGroupItem key={vault.name} value={vault.name}>
              {vault.name}
            </ToggleGroupItem>
          ))}
        </ToggleGroup>
      )}
      <Button variant="ghost" size="lg" onClick={() => setCreating(true)}>
        <PlusIcon data-icon="inline-start" />
        New vault
      </Button>
      {creating && (
        <CreateVaultDialog
          workspace={workspace}
          onClose={() => setCreating(false)}
          onCreated={(name) => {
            setCreating(false)
            onSelect(name)
          }}
        />
      )}
    </div>
  )
}

function CreateVaultDialog({
  workspace,
  onClose,
  onCreated,
}: {
  workspace: Workspace
  onClose: () => void
  onCreated: (name: string) => void
}) {
  const client = useQueryClient()
  const [name, setName] = useState("")
  const create = useMutation({
    mutationFn: (vault: string) =>
      createNamedVault(workspace.id, vault, workspace.kind === "team" ? workspace.id : undefined),
    onSuccess: async (_, vault) => {
      await client.invalidateQueries({ queryKey: namedVaultsKey(workspace.id) })
      onCreated(vault)
    },
  })
  const valid = validName(name)
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>New vault</DialogTitle>
        </DialogHeader>
        <form
          className="flex flex-col gap-4"
          onSubmit={(event) => {
            event.preventDefault()
            if (valid) create.mutate(name)
          }}
        >
          <Field className="gap-1.5">
            <FieldLabel htmlFor="vault-name">Name</FieldLabel>
            <Input
              id="vault-name"
              value={name}
              onChange={(event) => setName(event.target.value)}
              autoComplete="off"
              spellCheck={false}
              autoFocus
            />
            {create.isError ? (
              <FieldError>
                <ErrorMessage error={create.error} />
              </FieldError>
            ) : (
              <p className="text-xs text-muted-foreground">{nameHint}</p>
            )}
          </Field>
          <DialogFooter>
            <Button type="button" variant="ghost" size="lg" onClick={onClose}>
              Cancel
            </Button>
            <Button type="submit" size="lg" disabled={!valid || create.isPending}>
              {create.isPending ? "Creating…" : "Create"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

/** Under a named vault's variables: the recipes that read it, and its deletion (refused while one does). */
export function EnvVaultFooter({
  workspace,
  vault,
  onDeleted,
}: {
  workspace: Workspace
  vault: string
  onDeleted: () => void
}) {
  const client = useQueryClient()
  const vaults = useNamedVaults(workspace)
  const [confirming, setConfirming] = useState(false)
  const readers = vaults.data?.find((entry) => entry.name === vault)?.recipes
  return (
    <>
      {readers && (
        <p className="text-sm text-muted-foreground">
          {readers.length
            ? `Read by ${readers.length === 1 ? "recipe" : "recipes"} ${readers.join(", ")}.`
            : "No recipe reads this vault."}
        </p>
      )}
      <DangerZone
        title="Delete vault"
        description="Its variables are deleted for good."
        action="Delete"
        onAction={() => setConfirming(true)}
      >
        <ConfirmAction
          open={confirming}
          onOpenChange={setConfirming}
          title={`Delete ${vault}?`}
          description="Its variables are deleted for good."
          confirmName={vault}
          action="Delete"
          pending="Deleting…"
          mutation={{
            mutationFn: () => deleteNamedVault(workspace.id, vault),
            onSuccess: async () => {
              await client.invalidateQueries({ queryKey: namedVaultsKey(workspace.id) })
              onDeleted()
            },
          }}
        />
      </DangerZone>
    </>
  )
}
