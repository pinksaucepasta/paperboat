"use client"

import { KeyIcon, LockSimpleIcon } from "@phosphor-icons/react"
import { useQuery } from "@tanstack/react-query"
import { useSyncExternalStore } from "react"
import { meQuery, useApiQuery, useWorkspace, type Machine } from "@/lib/api"
import { lockVault, subscribeVaultLock, vaultUnlocked } from "@/lib/envvault/crypto"
import { readVault, type EnvScope } from "@/lib/envvault/vault"
import { CommandBlock } from "@/components/add-machine-dialog"
import { EnvUnlock } from "@/components/environment/env-unlock"
import { EnvVariables } from "@/components/environment/env-variables"
import { useCurrentMachine } from "@/components/machine-shell"
import { useListScope } from "@/components/list-toolbar"
import { Button } from "@/components/ui/button"
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { Skeleton } from "@/components/ui/skeleton"

/** The machine page's Environment tab: that machine's own variables. */
export function MachineEnvironment() {
  return <EnvironmentList machine={useCurrentMachine()} />
}

/**
 * Variable names and values, end to end encrypted: the vault password opens the vault in this
 * browser, names are listed, values are written and never shown. Setting up the vault, its
 * recovery code and password changes stay in `pb env vault`.
 */
export function EnvironmentList({ machine: tabMachine }: { machine?: Machine }) {
  const workspace = useWorkspace()
  const me = useApiQuery(meQuery)
  const { scope: machineId } = useListScope(tabMachine)
  const unlocked = useSyncExternalStore(subscribeVaultLock, vaultUnlocked, () => false)
  const vault = useQuery({ queryKey: ["env-vault"], queryFn: readVault, staleTime: 0 })
  if (!workspace || !me.data || vault.isPending) return <Skeleton className="h-40 w-full rounded-lg" />
  if (vault.isError) {
    return <p className="py-16 text-center text-sm text-destructive">{vault.error.message}</p>
  }
  if (!vault.data) return <NoVault />
  const team = workspace.kind === "team" && !machineId
  const scope: EnvScope = {
    kind: team ? "team" : "personal",
    owner: team ? workspace.id : me.data.id,
    machine: machineId ?? "",
    workspace: workspace.id,
  }
  const title = machineId ? "Variables for this machine" : team ? `${workspace.name} variables` : "Variables"
  if (!unlocked) return <EnvUnlock title={title} />
  return (
    <div className="flex flex-col gap-3">
      <div className="flex min-h-8 items-center gap-2">
        <h2 className="text-sm font-medium">{title}</h2>
        <Button variant="ghost" className="ml-auto text-muted-foreground" onClick={lockVault}>
          <LockSimpleIcon /> Lock
        </Button>
      </div>
      <EnvVariables key={JSON.stringify(scope)} scope={scope} />
    </div>
  )
}

function NoVault() {
  return (
    <Empty className="py-16">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <KeyIcon />
        </EmptyMedia>
        <EmptyTitle>Create your vault with pb</EmptyTitle>
        <EmptyDescription>
          The vault password is chosen on a machine. Then you can manage variables here.
        </EmptyDescription>
      </EmptyHeader>
      <div className="mx-auto flex w-full max-w-md flex-col gap-2">
        <CommandBlock command="pb env vault init" />
      </div>
    </Empty>
  )
}
