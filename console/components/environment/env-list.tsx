"use client"

import { KeyIcon } from "@phosphor-icons/react"
import { CommandBlock } from "@/components/add-machine-dialog"
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"

/** The machine page's Environment tab. */
export function MachineEnvironment() {
  return <EnvironmentList />
}

/**
 * Environment variables are encrypted on the client with keys from the vault password, so the
 * console can't list or edit them until the vault opens in the browser. Until then the CLI is
 * the one place to manage them.
 */
export function EnvironmentList() {
  return (
    <Empty className="py-16">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <KeyIcon />
        </EmptyMedia>
        <EmptyTitle>Manage variables with pb</EmptyTitle>
        <EmptyDescription>
          Values are encrypted end to end, so they’re set and listed from a machine, not here yet.
        </EmptyDescription>
      </EmptyHeader>
      <div className="mx-auto flex w-full max-w-md flex-col gap-2">
        <CommandBlock command="pb env set API_KEY" />
        <CommandBlock command="pb env list" />
      </div>
    </Empty>
  )
}
