"use client"

import { PlusIcon, TrashIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { toast } from "sonner"
import { clientConfigQuery, useApiQuery, useMachine, useWorkspace } from "@/lib/api"
import {
  portNameAddress,
  portNamePattern,
  portNamesQuery,
  removePortName,
  setPortName,
  validPort,
  type PortName,
  type PortNameScope,
} from "@/lib/port-names"
import { ConfirmAction } from "@/components/list-toolbar"
import { LoadError } from "@/components/load-error"
import { MachinePicker } from "@/components/machine-picker"
import { RowMenus, type Entry } from "@/components/row-menus"
import { SectionHeading } from "@/components/section"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldError, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { ErrorMessage } from "@/components/error-reference"
import { useShellState } from "@/lib/use-shell-state"

const scopeLabels: Record<PortNameScope, string> = {
  machine: "One machine",
  all_machines: "All machines",
  top_level: "Top level",
}

/** Names for ports, as part of the Network page: each row is the address it opens in a browser. */
export function PortNames() {
  const workspace = useWorkspace()
  const ws = workspace?.id ?? ""
  const list = useApiQuery({ ...portNamesQuery(ws), enabled: Boolean(workspace) })
  const domain = useApiQuery(clientConfigQuery).data?.local_access_base_domain ?? ""
  const [adding, setAdding] = useShellState("network/port-names.tsx:adding", false)
  // Owners and admins (everyone in a personal workspace) set names that cover more than one machine.
  const wide = workspace?.kind === "personal" || workspace?.role === "owner" || workspace?.role === "admin"

  return (
    <section id="port-names" className="flex flex-col gap-3">
      <SectionHeading
        title="Port names"
        action={
          <Button size="lg" onClick={() => setAdding(true)}>
            <PlusIcon data-icon="inline-start" />
            Add name
          </Button>
        }
      />
      {list.isError ? (
        <LoadError what="port names" error={list.error} retry={() => list.refetch()} />
      ) : list.isPending ? (
        <div className="flex flex-col border-y border-divider">
          <div className="flex min-h-14 items-center gap-3 border-b border-divider py-2">
            <Skeleton className="my-0.5 h-4 w-56" />
          </div>
        </div>
      ) : list.data.length === 0 ? (
        <p className="border-y border-divider py-3 text-sm text-muted-foreground">
          None. Add a name to open a port in a browser, like jellyfin.{domain || "example.com"}.
        </p>
      ) : (
        <div className="flex flex-col divide-y divide-divider border-y border-divider">
          {list.data.map((name) => (
            <NameRow key={name.id} name={name} domain={domain} wide={wide} />
          ))}
        </div>
      )}
      {adding && <AddNameDialog wide={wide} domain={domain} onClose={() => setAdding(false)} />}
    </section>
  )
}

const target = (name: PortName) =>
  name.scope === "all_machines" ? "every machine" : (name.machine_alias ?? "a machine")

function NameRow({ name, domain, wide }: { name: PortName; domain: string; wide: boolean }) {
  const workspace = useWorkspace()
  const queryClient = useQueryClient()
  const [removing, setRemoving] = useState(false)
  const address = portNameAddress(name, domain)
  const machineScoped = name.scope === "machine"
  // Machine names are managed by whoever manages that machine; wide names by owners and admins.
  const machine = useMachine(machineScoped && !wide ? name.machine_id : undefined).data
  const canRemove = machineScoped ? wide || machine?.can_manage === true : wide
  const entries: Entry[] = [
    { kind: "item", label: "Remove name", icon: TrashIcon, destructive: true, onSelect: () => setRemoving(true) },
  ]
  const line = (
    <div className="flex min-h-14 min-w-0 items-center gap-4 px-3 py-2 text-sm">
      <div className="flex min-w-0 flex-1 flex-col justify-center">
        <span className="truncate font-mono text-code font-medium">{address}</span>
        {name.shadowed && (
          <span className="truncate text-xs text-muted-foreground">
            A machine named {name.name} now uses this address.
          </span>
        )}
      </div>
      <span className="hidden w-40 shrink-0 truncate text-muted-foreground sm:block">
        {scopeLabels[name.scope]}
        {name.scope !== "all_machines" && <> · {target(name)}</>}
      </span>
      <span className="w-16 shrink-0 text-right font-mono text-code">{name.port}</span>
      <span className="flex w-8 shrink-0 justify-end" />
    </div>
  )
  const dialog = (
    <ConfirmAction
      open={removing}
      onOpenChange={setRemoving}
      title={`Remove ${address}?`}
      description="The address stops opening this port. The port itself keeps running."
      action="Remove name"
      pending="Removing…"
      mutation={{
        mutationFn: () => removePortName(workspace?.id ?? "", name.id),
        onSuccess: async () => {
          await queryClient.invalidateQueries({ queryKey: ["port-names"] })
          toast.success(`${address} removed`)
        },
      }}
    />
  )
  if (!canRemove) return line
  return (
    <RowMenus
      label={`Actions for ${address}`}
      entries={entries}
      dialogs={dialog}
      row={(actions) => (
        <div className="flex items-center transition-colors hover:bg-surface data-popup-open:bg-surface">
          <div className="min-w-0 flex-1">{line}</div>
          <div className="w-12 shrink-0">{actions}</div>
        </div>
      )}
    />
  )
}

function AddNameDialog({ wide, domain, onClose }: { wide: boolean; domain: string; onClose: () => void }) {
  const workspace = useWorkspace()
  const queryClient = useQueryClient()
  const [scope, setScope] = useShellState<PortNameScope>("network/port-names.tsx:scope", "machine")
  const [draft, setDraft] = useShellState("network/port-names.tsx:draft", "")
  const [portText, setPortText] = useShellState("network/port-names.tsx:portText", "")
  const [machineId, setMachineId] = useShellState<string | null>("network/port-names.tsx:machineId", null)
  const name = draft.trim().toLowerCase()
  const port = Number(portText)
  const picked = useMachine(machineId).data
  const needsMachine = scope !== "all_machines"
  const nameProblem =
    name && !portNamePattern.test(name) ? "Use lowercase letters, digits and dashes. Not only digits." : undefined
  const portProblem = portText && !validPort(port) ? "A port is a number from 1 to 65535." : undefined
  const denied =
    scope === "machine" && picked && !wide && !picked.can_manage ? "You can’t manage this machine." : undefined
  const ready = name && !nameProblem && portText && !portProblem && (!needsMachine || machineId) && !denied
  const add = useMutation({
    mutationFn: () =>
      setPortName(workspace?.id ?? "", {
        scope,
        name,
        port,
        ...(needsMachine && machineId ? { machine_id: machineId } : {}),
      }),
    onSuccess: async (record) => {
      await queryClient.invalidateQueries({ queryKey: ["port-names"] })
      toast.success(`${portNameAddress(record, domain)} added`)
      onClose()
    },
  })
  const error = nameProblem ?? portProblem ?? denied ?? (add.isError ? <ErrorMessage error={add.error} /> : undefined)
  return (
    <Dialog open onOpenChange={(next) => !next && !add.isPending && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Add a port name</DialogTitle>
        </DialogHeader>
        <form
          className="flex flex-col gap-4"
          onSubmit={(event) => {
            event.preventDefault()
            if (ready) add.mutate()
          }}
        >
          <Field className="gap-1.5">
            <FieldLabel>Applies to</FieldLabel>
            <ToggleGroup
              value={[scope]}
              onValueChange={(next) => next[0] && setScope(next[0] as PortNameScope)}
              variant="outline"
              spacing={0}
            >
              <ToggleGroupItem value="machine">{scopeLabels.machine}</ToggleGroupItem>
              <ToggleGroupItem value="all_machines" disabled={!wide}>
                {scopeLabels.all_machines}
              </ToggleGroupItem>
              <ToggleGroupItem value="top_level" disabled={!wide}>
                {scopeLabels.top_level}
              </ToggleGroupItem>
            </ToggleGroup>
            {!wide && (
              <span className="text-xs text-muted-foreground">
                Names for all machines or at the top level need a workspace owner or admin.
              </span>
            )}
          </Field>
          {needsMachine && (
            <Field className="gap-1.5">
              <FieldLabel>Machine</FieldLabel>
              <MachinePicker value={machineId} onChange={setMachineId} />
            </Field>
          )}
          <Field data-invalid={Boolean(nameProblem)} className="gap-1.5">
            <FieldLabel htmlFor="port-name">Name</FieldLabel>
            <Input
              id="port-name"
              autoFocus
              placeholder="jellyfin"
              value={draft}
              onChange={(event) => setDraft(event.target.value)}
              aria-invalid={Boolean(nameProblem)}
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
            />
          </Field>
          <Field data-invalid={Boolean(portProblem)} className="gap-1.5">
            <FieldLabel htmlFor="port-name-port">Port</FieldLabel>
            <Input
              id="port-name-port"
              inputMode="numeric"
              placeholder="8096"
              value={portText}
              onChange={(event) => setPortText(event.target.value)}
              aria-invalid={Boolean(portProblem)}
              autoComplete="off"
              className="font-mono"
            />
            {error ? (
              <FieldError>{error}</FieldError>
            ) : (
              name &&
              domain && (
                <span className="truncate font-mono text-xs text-muted-foreground">
                  {portNameAddress(
                    { id: "", scope, name, port: 0, machine_alias: picked?.alias, shadowed: false },
                    domain,
                  )}
                </span>
              )
            )}
          </Field>
          <DialogFooter>
            <Button type="button" variant="secondary" size="lg" onClick={onClose} disabled={add.isPending}>
              Cancel
            </Button>
            <Button type="submit" size="lg" disabled={!ready || add.isPending}>
              {add.isPending ? "Adding…" : "Add name"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
