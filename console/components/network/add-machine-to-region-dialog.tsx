"use client"

import { CheckCircleIcon, CircleNotchIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useEffect, useState } from "react"
import { clientConfigQuery, useApiQuery } from "@/lib/api"
import {
  installCommand,
  installationsQuery,
  pairNode,
  refreshNetwork,
  roleLabel,
  type Capability,
  type Installation,
  type Region,
  type scopeOf,
} from "@/lib/network"
import { CommandBlock } from "@/components/add-machine-dialog"
import { StatusDot } from "@/components/machine-status"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { ErrorMessage } from "@/components/error-reference"

export type Role = "relay" | "edge"

/** The capability the server knows an edge by. */
const capabilityOf = (role: Role): Capability => (role === "edge" ? "tunnel" : "relay")

/**
 * Add relay / Add edge to a region (the region and the role are fixed): 1) the installer command, which carries both,
 * 2) the address and one-time code it prints, paired with the region and role the server must find, 3) a live wait
 * that polls the region's machines until the new one reports ready. Closing the dialog during the wait is fine:
 * the machine appears in the region's table anyway.
 */
export function AddMachineToRegionDialog({
  scope,
  region,
  role,
  onClose,
}: {
  scope: ReturnType<typeof scopeOf>
  region: Region
  role: Role
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  const installer = useApiQuery(clientConfigQuery).data?.installer_base_url
  const [address, setAddress] = useState("")
  const [code, setCode] = useState("")
  const pair = useMutation({
    mutationFn: () =>
      pairNode({
        address: address.trim(),
        code: code.trim(),
        scope,
        expectRegion: region.id,
        expectCapability: capabilityOf(role),
      }),
    onSuccess: () => refreshNetwork(queryClient),
  })
  const ready = Boolean(address.trim() && code.trim())
  const title = `Add ${role} to ${region.name}`

  return (
    <Dialog open onOpenChange={(next) => !next && !pair.isPending && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
        </DialogHeader>
        {pair.data ? (
          <WaitForMachine scope={scope} region={region} role={role} paired={pair.data} onClose={onClose} />
        ) : (
          <form
            className="flex min-w-0 flex-col gap-4"
            onSubmit={(event) => {
              event.preventDefault()
              if (ready && !pair.isPending) pair.mutate()
            }}
          >
            <div className="flex flex-col gap-2">
              {installer ? (
                <CommandBlock command={installCommand(installer, role, region.id)} />
              ) : (
                <Skeleton className="h-12 w-full" />
              )}
              <p className="text-xs text-muted-foreground">Run this on a Linux server with a public IP.</p>
            </div>

            <div className="flex flex-col gap-3 border-t border-divider pt-4">
              <p className="text-sm font-medium">Paste what the installer printed</p>
              <Field className="gap-1.5">
                <FieldLabel htmlFor="node-address">Address</FieldLabel>
                <Input
                  id="node-address"
                  placeholder="203.0.113.7:7277"
                  value={address}
                  onChange={(event) => setAddress(event.target.value)}
                  className="font-mono"
                  autoComplete="off"
                  spellCheck={false}
                />
              </Field>
              <Field className="gap-1.5">
                <FieldLabel htmlFor="node-code">Code</FieldLabel>
                <Input
                  id="node-code"
                  value={code}
                  onChange={(event) => setCode(event.target.value)}
                  className="font-mono"
                  autoComplete="off"
                  spellCheck={false}
                />
                <p className="text-xs text-muted-foreground">The code works once, for 15 minutes.</p>
              </Field>
            </div>
            {pair.isError && (
              <p className="text-sm text-destructive">
                <ErrorMessage error={pair.error} />
              </p>
            )}
            <DialogFooter>
              <Button type="button" variant="secondary" size="lg" onClick={onClose} disabled={pair.isPending}>
                Cancel
              </Button>
              <Button type="submit" size="lg" disabled={!ready || pair.isPending}>
                {pair.isPending ? "Pairing…" : `Pair ${role}`}
              </Button>
            </DialogFooter>
          </form>
        )}
      </DialogContent>
    </Dialog>
  )
}

/**
 * Step 3: the claim returned, now the machine has to report. Polls the scope's installations every 3 seconds and finds
 * the paired one by its installation ID (or, when the claim named none, the region's newest enrolled one of this role).
 */
function WaitForMachine({
  scope,
  region,
  role,
  paired,
  onClose,
}: {
  scope: ReturnType<typeof scopeOf>
  region: Region
  role: Role
  paired: Installation[]
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  const ids = paired.map((installation) => installation.installation_id)
  const list = useApiQuery({
    ...installationsQuery(scope, ids[0] ?? "", 1),
    refetchInterval: 3000,
  })
  const found =
    (list.data?.items ?? []).find((installation) => ids.includes(installation.installation_id)) ??
    (ids.length === 0
      ? list.data?.items.find(
          (installation) =>
            installation.region === region.id &&
            installation.capability === capabilityOf(role) &&
            installation.enrollment_state === "enrolled",
        )
      : undefined)
  // The claim's own answer counts until the list has the installation.
  const machine = found ?? paired[0]
  const online = Boolean(machine?.ready)
  useEffect(() => {
    if (online) void refreshNetwork(queryClient)
  }, [online, queryClient])

  if (online)
    return (
      <div className="flex flex-col items-center gap-3 py-4 text-center">
        <CheckCircleIcon weight="fill" className="size-10 text-success" />
        <p className="flex items-center gap-2 font-medium">
          {machine.name}
          <StatusDot online />
        </p>
        <p className="text-sm text-muted-foreground">
          {roleLabel(machine.capability)} in {region.name}: Ready
        </p>
        <Button size="lg" className="mt-1" onClick={onClose}>
          Done
        </Button>
      </div>
    )
  const enrolled = machine?.enrollment_state === "enrolled"
  return (
    <div className="flex min-w-0 flex-col gap-4">
      <div className="flex items-center gap-3 py-2" role="status">
        <CircleNotchIcon className="size-5 shrink-0 animate-spin text-muted-foreground" />
        <div className="flex flex-col gap-0.5">
          <p className="text-sm font-medium">Waiting for the machine to connect</p>
          {enrolled && (
            <p className="text-xs text-muted-foreground">Pairing finished; the machine has not reported yet.</p>
          )}
        </div>
      </div>
      <div className="flex flex-col gap-2 border-t border-divider pt-4">
        <p className="text-xs text-muted-foreground">Not responding? Run this on the machine to check it.</p>
        <CommandBlock command="pbh status" />
      </div>
      <DialogFooter>
        <Button type="button" variant="secondary" size="lg" onClick={onClose}>
          Close
        </Button>
      </DialogFooter>
    </div>
  )
}
