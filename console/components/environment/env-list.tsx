"use client"

import { KeyIcon, LockSimpleIcon, PlusIcon } from "@phosphor-icons/react"
import { useQuery } from "@tanstack/react-query"
import { useState, useSyncExternalStore } from "react"
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
import { SectionSkeleton } from "@/components/skeletons"
import { PageIntro, Section } from "@/components/section"
import { EnvNamesSwitch } from "@/components/environment/env-names-switch"

/** The machine page's Environment tab: that machine's own variables. */
export function MachineEnvironment() {
  return <EnvironmentList machine={useCurrentMachine()} />
}

/**
 * Environment page: what the page is, the variables (an inline unlock card while locked), the
 * workspace's settings and how machines use the values. Add variable sits in the header once the
 * vault is open.
 */
export function EnvironmentPage() {
  const [adding, setAdding] = useState(false)
  const unlocked = useSyncExternalStore(subscribeVaultLock, vaultUnlocked, () => false)
  const vault = useQuery({ queryKey: ["env-vault"], queryFn: ({ signal }) => readVault(signal), staleTime: 0 })
  return (
    <div className="flex flex-col gap-10">
      <PageIntro
        title="Environment"
        description="End-to-end encrypted variables, delivered to your machines’ shells and commands."
        action={
          unlocked &&
          vault.data && (
            <Button size="lg" onClick={() => setAdding(true)}>
              <PlusIcon data-icon="inline-start" />
              Add variable
            </Button>
          )
        }
      />
      <EnvironmentList adding={adding} onAddingChange={setAdding} />
      <EnvNamesSwitch />
      <Section title="Use them">
        <div className="flex flex-col gap-4 pt-3 text-sm">
          <div className="flex flex-col gap-1.5">
            <span className="text-muted-foreground">Run one command with your variables:</span>
            <CommandBlock command="pb env run -- <command>" />
          </div>
          <div className="flex flex-col gap-1.5">
            <span className="text-muted-foreground">
              Or add them to your shell: install the hook once, then run pb env load in a new shell.
            </span>
            <CommandBlock command="pb env hook --install" />
          </div>
        </div>
      </Section>
    </div>
  )
}

/**
 * Variable names and values, end to end encrypted: the vault password opens the vault in this
 * browser, names are listed, values are written and never shown. Setting up the vault, its
 * recovery code and password changes stay in `pb env vault`. On the Environment page Add variable
 * lives in the header (`adding`); on a machine's tab it sits above the list.
 */
export function EnvironmentList({
  machine: tabMachine,
  adding,
  onAddingChange,
}: {
  machine?: Machine
  adding?: boolean
  onAddingChange?: (adding: boolean) => void
}) {
  const workspace = useWorkspace()
  const me = useApiQuery(meQuery)
  const { scope: machineId } = useListScope(tabMachine)
  const unlocked = useSyncExternalStore(subscribeVaultLock, vaultUnlocked, () => false)
  const vault = useQuery({ queryKey: ["env-vault"], queryFn: ({ signal }) => readVault(signal), staleTime: 0 })
  if (!workspace || !me.data) return <SectionSkeleton title="Variables" rows={2} />
  // Most visits find the vault locked, so it loads as the unlock card (typing works, Unlock waits).
  if (vault.isPending) return <EnvUnlock loading />
  if (vault.isError) {
    return <p className="py-16 text-center text-sm text-destructive">{vault.error.message}</p>
  }
  if (!vault.data) return <NoVault />
  if (!unlocked) return <EnvUnlock />
  const team = workspace.kind === "team" && !machineId
  const scope: EnvScope = {
    kind: team ? "team" : "personal",
    owner: team ? workspace.id : me.data.id,
    machine: machineId ?? "",
    workspace: workspace.id,
  }
  const title = machineId ? "Variables for this machine" : team ? `${workspace.name} variables` : "Variables"
  return (
    <Section
      title={title}
      action={
        <Button variant="ghost" className="text-muted-foreground" onClick={lockVault}>
          <LockSimpleIcon /> Lock
        </Button>
      }
    >
      <EnvVariables key={JSON.stringify(scope)} scope={scope} adding={adding} onAddingChange={onAddingChange} />
    </Section>
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
