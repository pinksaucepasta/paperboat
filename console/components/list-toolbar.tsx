"use client"

import { MagnifyingGlassIcon, XIcon } from "@phosphor-icons/react"
import { useMutation, type UseMutationOptions } from "@tanstack/react-query"
import { useSearchParams } from "next/navigation"
import { useState } from "react"
import { useMachines, type Machine } from "@/lib/api"
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
import { InputGroup, InputGroupAddon, InputGroupInput } from "@/components/ui/input-group"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"

/*
 * Pieces shared by the machine-scoped lists (Terminals, Previews, Tunnels), so they look and
 * behave the same: the search field, the view toggle with counts, the machine scope, and the
 * confirmation for destructive row actions.
 */

/**
 * The machine a list is narrowed to: the machine page's own tab, or the machine picked in the
 * section header (`?machine=`). `machine` is undefined while the machines load or for a machine
 * outside this workspace; `scope` still filters by its ID.
 */
export function useListScope(tabMachine?: Machine) {
  const picked = useSearchParams().get("machine")
  const machines = useMachines()
  const scope = tabMachine?.id ?? picked
  const machine = tabMachine ?? machines.data?.find((entry) => entry.id === picked)
  const aliasOf = (id: string) => machines.data?.find((entry) => entry.id === id)?.alias
  return { scope, machine, aliasOf }
}

export function SearchField({
  value,
  onChange,
  placeholder,
}: {
  value: string
  onChange: (value: string) => void
  placeholder: string
}) {
  return (
    <InputGroup className="h-8 max-w-sm border-transparent bg-surface dark:bg-surface">
      <InputGroupAddon>
        <MagnifyingGlassIcon className="size-4" />
      </InputGroupAddon>
      <InputGroupInput
        className="md:text-sm"
        placeholder={placeholder}
        value={value}
        onChange={(event) => onChange(event.target.value)}
        spellCheck={false}
      />
      {value && (
        <InputGroupAddon align="inline-end">
          <button
            type="button"
            aria-label="Clear search"
            onClick={() => onChange("")}
            className="text-muted-foreground outline-none hover:text-foreground"
          >
            <XIcon className="size-3.5" />
          </button>
        </InputGroupAddon>
      )}
    </InputGroup>
  )
}

/** Two or three views of one list (Open / Closed, Live / Ended) with their counts. */
export function ViewToggle<View extends string>({
  value,
  onChange,
  views,
}: {
  value: View
  onChange: (view: View) => void
  views: { value: View; label: string; count?: number }[]
}) {
  return (
    <ToggleGroup
      value={[value]}
      onValueChange={(next) => next[0] && onChange(next[0] as View)}
      variant="outline"
      spacing={0}
    >
      {views.map((view) => (
        <ToggleGroupItem key={view.value} value={view.value} className="gap-1.5">
          {view.label} <span className="text-muted-foreground tabular-nums">{view.count ?? ""}</span>
        </ToggleGroupItem>
      ))}
    </ToggleGroup>
  )
}

/**
 * Confirms a destructive action and runs it. The dialog stays open while it runs and shows the
 * server's error in place if it fails, so nothing looks done before it is. Removing an object
 * for good passes `confirmName`, which must be typed first (DESIGN.md: Danger zone).
 */
export function ConfirmAction<TData>({
  open,
  onOpenChange,
  title,
  description,
  action,
  pending,
  mutation,
  confirmName,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  title: string
  description: React.ReactNode
  action: string
  pending: string
  mutation: UseMutationOptions<TData, Error, void>
  confirmName?: string
}) {
  const [typed, setTyped] = useState("")
  const confirmed = !confirmName || typed.trim() === confirmName
  const run = useMutation({
    ...mutation,
    onSuccess: async (...args) => {
      await mutation.onSuccess?.(...args)
      onOpenChange(false)
    },
  })
  return (
    <AlertDialog
      open={open}
      onOpenChange={(next) => {
        if (run.isPending) return
        if (!next) {
          run.reset()
          setTyped("")
        }
        onOpenChange(next)
      }}
    >
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{title}</AlertDialogTitle>
          <AlertDialogDescription>{description}</AlertDialogDescription>
        </AlertDialogHeader>
        {confirmName && (
          <form
            onSubmit={(event) => {
              event.preventDefault()
              if (confirmed) run.mutate()
            }}
          >
            <Field className="gap-1.5">
              <FieldLabel htmlFor="confirm-name" className="font-normal text-muted-foreground">
                Type <span className="font-mono font-medium text-foreground">{confirmName}</span> to confirm
              </FieldLabel>
              <Input
                id="confirm-name"
                autoFocus
                value={typed}
                onChange={(event) => setTyped(event.target.value)}
                autoComplete="off"
                spellCheck={false}
              />
            </Field>
          </form>
        )}
        {run.isError && <p className="text-sm text-destructive">{run.error.message}</p>}
        <AlertDialogFooter>
          <AlertDialogCancel disabled={run.isPending}>Cancel</AlertDialogCancel>
          <AlertDialogAction
            variant="destructive"
            disabled={!confirmed || run.isPending}
            onClick={(event) => {
              event.preventDefault()
              if (confirmed) run.mutate()
            }}
          >
            {run.isPending ? pending : action}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
