"use client"

import { KeyIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { unlockVault } from "@/lib/envvault/vault"
import { Button } from "@/components/ui/button"
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { Field, FieldError } from "@/components/ui/field"
import { Input } from "@/components/ui/input"

/** The password opens the vault inside this browser; it is cleared as soon as it is submitted. */
export function EnvUnlock({ title }: { title: string }) {
  const [password, setPassword] = useState("")
  const client = useQueryClient()
  const unlock = useMutation({
    mutationFn: (secret: string) => unlockVault(secret),
    onSuccess: () => client.invalidateQueries({ queryKey: ["env-names"] }),
  })
  return (
    <Empty className="py-16">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <KeyIcon />
        </EmptyMedia>
        <EmptyTitle>{title}</EmptyTitle>
        <EmptyDescription>Unlock with your vault password. It never leaves this browser.</EmptyDescription>
      </EmptyHeader>
      <form
        className="mx-auto flex w-full max-w-xs flex-col gap-2"
        onSubmit={(event) => {
          event.preventDefault()
          const secret = password
          setPassword("")
          if (secret) unlock.mutate(secret)
        }}
      >
        <Field className="gap-1.5">
          <Input
            type="password"
            name="vault-password"
            autoComplete="off"
            aria-label="Vault password"
            placeholder="Vault password"
            value={password}
            onChange={(event) => setPassword(event.target.value)}
            autoFocus
          />
          {unlock.isError && <FieldError>{unlock.error.message}</FieldError>}
        </Field>
        <Button type="submit" size="lg" disabled={!password || unlock.isPending}>
          {unlock.isPending ? "Unlocking…" : "Unlock"}
        </Button>
      </form>
    </Empty>
  )
}
