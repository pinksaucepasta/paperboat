"use client"

import { LockSimpleIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { unlockVault } from "@/lib/envvault/vault"
import { Button } from "@/components/ui/button"
import { FieldError } from "@/components/ui/field"
import { Input } from "@/components/ui/input"

/**
 * An inline card at the top of the variables: the password opens the vault inside this browser and
 * is cleared as soon as it is submitted. The rest of the page stays visible below it. `loading`: the
 * vault is still being read; typing works, unlocking waits.
 */
export function EnvUnlock({
  loading,
  title = "Variables are encrypted. Unlock to see and edit them.",
}: {
  loading?: boolean
  /** What unlocking is for, on pages other than ENV. */
  title?: string
}) {
  const [password, setPassword] = useState("")
  const client = useQueryClient()
  const unlock = useMutation({
    mutationFn: (secret: string) => unlockVault(secret),
    onSuccess: () => client.invalidateQueries({ queryKey: ["env-names"] }),
  })
  return (
    <div className="flex flex-col gap-3 surface rounded-lg p-4 text-sm">
      <div className="flex flex-col gap-3 md:flex-row md:items-center">
        <div className="flex min-w-0 flex-1 items-start gap-3">
          <LockSimpleIcon className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
          <div className="flex flex-col gap-0.5">
            <span>{title}</span>
            <span className="text-muted-foreground">Your vault password never leaves this browser.</span>
          </div>
        </div>
        <form
          className="flex gap-2 md:w-96"
          onSubmit={(event) => {
            event.preventDefault()
            const secret = password
            setPassword("")
            if (secret) unlock.mutate(secret)
          }}
        >
          <Input
            type="password"
            name="vault-password"
            autoComplete="off"
            aria-label="Vault password"
            placeholder="Vault password"
            value={password}
            onChange={(event) => setPassword(event.target.value)}
            className="h-8 flex-1"
          />
          <Button type="submit" size="lg" disabled={loading || !password || unlock.isPending}>
            {unlock.isPending ? "Unlocking…" : "Unlock"}
          </Button>
        </form>
      </div>
      {unlock.isError && <FieldError>{unlock.error.message}</FieldError>}
    </div>
  )
}
