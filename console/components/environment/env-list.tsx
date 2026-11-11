"use client"

import { LockSimpleIcon, PlusIcon } from "@phosphor-icons/react"
import { useQuery } from "@tanstack/react-query"
import { useState, useSyncExternalStore } from "react"
import { meQuery, useApiQuery, useWorkspace, type Machine } from "@/lib/api"
import { lockVault, subscribeVaultLock, vaultUnlocked } from "@/lib/envvault/crypto"
import { readVault, type EnvScope } from "@/lib/envvault/vault"
import { CommandBlock } from "@/components/add-machine-dialog"
import { EnvUnlock } from "@/components/environment/env-unlock"
import { EnvVariables } from "@/components/environment/env-variables"
import { EnvVaultSettings } from "@/components/environment/env-vault-settings"
import { EnvVaultSetup } from "@/components/environment/env-vault-setup"
import { useCurrentMachine } from "@/components/machine-shell"
import { useListScope } from "@/components/list-toolbar"
import { Button } from "@/components/ui/button"
import { Section } from "@/components/section"
import { EnvNamesSwitch } from "@/components/environment/env-names-switch"

/** The machine page's Environment tab: that machine's own variables. */
export function MachineEnvironment() {
  return <EnvironmentList machine={useCurrentMachine()} />
}

/**
 * Environment page: the variables (an inline unlock card while locked; Add variable in their
 * Section header once open), the workspace's settings and how machines use the values.
 */
export function EnvironmentPage() {
  const [adding, setAdding] = useState(false)
  const unlocked = useSyncExternalStore(subscribeVaultLock, vaultUnlocked, () => false)
  return (
    <div className="flex flex-col gap-10">
      <EnvironmentList adding={adding} onAddingChange={setAdding} />
      <EnvNamesSwitch />
      {unlocked && <EnvVaultSettings />}
      <Section title="Use them">
        <div className="flex flex-col gap-4 pt-3 text-sm">
          <div className="flex flex-col gap-1.5">
            <span className="text-muted-foreground">Run one command with your variables:</span>
            <CommandBlock command="pb env run -- <command>" />
          </div>
          <div className="flex flex-col gap-1.5">
            <span className="text-muted-foreground">
              Or add them to your shell: install the hook once, then run{" "}
              <code className="font-mono text-code text-foreground">pb env load</code> in a new shell.
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
 * browser, names are listed, values are written and never shown. Without a vault the page sets one up;
 * a forgotten password is recovered from the unlock card. On the Environment page Add variable
 * is the Section's action (`adding`); on a machine's tab it sits above the list.
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
  // Most visits find the vault locked, so it loads as the unlock card (typing works, Unlock waits),
  // in the static shell too, before the session is known.
  if (!workspace || !me.data || vault.isPending) return <EnvUnlock loading />
  if (vault.isError) {
    return <p className="py-16 text-center text-sm text-destructive">{vault.error.message}</p>
  }
  if (!vault.data) return <EnvVaultSetup />
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
        <span className="flex items-center gap-2">
          <Button variant="ghost" className="text-muted-foreground" onClick={lockVault}>
            <LockSimpleIcon /> Lock
          </Button>
          {onAddingChange && (
            <Button size="lg" onClick={() => onAddingChange(true)}>
              <PlusIcon data-icon="inline-start" />
              Add variable
            </Button>
          )}
        </span>
      }
    >
      <EnvVariables key={JSON.stringify(scope)} scope={scope} adding={adding} onAddingChange={onAddingChange} />
    </Section>
  )
}
