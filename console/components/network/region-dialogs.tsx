"use client"

import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { toast } from "sonner"
import { createRegion, deleteRegion, refreshNetwork, renameRegion, type Region, type scopeOf } from "@/lib/network"
import { ConfirmAction } from "@/components/list-toolbar"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { ErrorMessage } from "@/components/error-reference"

type Scope = ReturnType<typeof scopeOf>

/**
 * New region (no `region`) and Rename (with it): one field, the name. The server answers 409 `region_exists` with the
 * region that already has the name; its sentence is shown under the field. Renaming never touches the slug.
 */
export function RegionNameDialog({ scope, region, onClose }: { scope: Scope; region?: Region; onClose: () => void }) {
  const queryClient = useQueryClient()
  const [name, setName] = useState(region?.name ?? "")
  const trimmed = name.trim()
  const save = useMutation({
    mutationFn: () => (region ? renameRegion(scope, region, trimmed) : createRegion(scope, trimmed)),
    onSuccess: async (saved) => {
      await refreshNetwork(queryClient)
      toast.success(region ? `Renamed to ${saved.name}` : `${saved.name} created`)
      onClose()
    },
  })
  const ready = trimmed.length > 0 && trimmed.length <= 48 && trimmed !== region?.name
  return (
    <Dialog open onOpenChange={(open) => !open && !save.isPending && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{region ? `Rename ${region.name}` : "New region"}</DialogTitle>
        </DialogHeader>
        <form
          className="flex min-w-0 flex-col gap-4"
          onSubmit={(event) => {
            event.preventDefault()
            if (ready && !save.isPending) save.mutate()
          }}
        >
          <Field className="gap-1.5">
            <FieldLabel htmlFor="region-name">Region name</FieldLabel>
            <Input
              id="region-name"
              autoFocus
              placeholder="Home lab"
              maxLength={48}
              value={name}
              onChange={(event) => {
                save.reset()
                setName(event.target.value)
              }}
              autoComplete="off"
              spellCheck={false}
            />
            <p className="text-xs text-muted-foreground">Machines you add to a region back each other up.</p>
          </Field>
          {save.isError && (
            <p className="text-sm text-destructive">
              <ErrorMessage error={save.error} />
            </p>
          )}
          <DialogFooter>
            <Button type="button" variant="secondary" size="lg" onClick={onClose} disabled={save.isPending}>
              Cancel
            </Button>
            <Button type="submit" size="lg" disabled={!ready || save.isPending}>
              {save.isPending ? "Saving…" : region ? "Rename" : "Create region"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

/**
 * Removes a region after its name is typed. A region with machines, or one a workspace uses, is refused (409
 * `region_in_use`): the server's sentence says what to do first, and ConfirmAction shows it in place.
 */
export function RemoveRegionAction({
  scope,
  region,
  open,
  onOpenChange,
}: {
  scope: Scope
  region: Region
  open: boolean
  onOpenChange: (open: boolean) => void
}) {
  const queryClient = useQueryClient()
  return (
    <ConfirmAction
      open={open}
      onOpenChange={onOpenChange}
      title={`Remove ${region.name}?`}
      description="Only a region with no machines that no workspace uses can be removed."
      action="Remove region"
      pending="Removing…"
      confirmName={region.name}
      mutation={{
        mutationFn: () => deleteRegion(scope, region),
        onSuccess: async () => {
          await refreshNetwork(queryClient)
          toast.success(`${region.name} removed`)
        },
      }}
    />
  )
}
