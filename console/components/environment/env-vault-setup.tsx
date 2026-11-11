"use client"

import { KeyIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { newRecoveryCode, setUpVault } from "@/lib/envvault/protection"
import { PasswordForm, RecoveryCodeStep } from "@/components/environment/env-recovery-code"
import { Empty, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"

/** No vault yet: a password and a recovery code create one in this browser, the same vault `pb env vault init` makes. */
export function EnvVaultSetup() {
  const client = useQueryClient()
  const [made, setMade] = useState<{ password: string; code: string }>()
  const prepare = useMutation({
    mutationFn: async (password: string) => ({ password, code: await newRecoveryCode() }),
    onSuccess: setMade,
  })
  const create = useMutation({
    mutationFn: () => setUpVault(made!.password, made!.code),
    onSuccess: () =>
      Promise.all([
        client.invalidateQueries({ queryKey: ["env-names"] }),
        client.invalidateQueries({ queryKey: ["env-vault"] }),
      ]),
  })
  return (
    <Empty className="py-16">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <KeyIcon />
        </EmptyMedia>
        <EmptyTitle>Create your vault</EmptyTitle>
      </EmptyHeader>
      <div className="mx-auto flex w-full max-w-md flex-col gap-2 text-left">
        {made ? (
          <RecoveryCodeStep
            code={made.code}
            confirmLabel="Create vault"
            pending={create.isPending}
            error={create.error}
            onConfirm={() => create.mutate()}
            onCancel={() => setMade(undefined)}
          />
        ) : (
          <PasswordForm
            submitLabel="Continue"
            pending={prepare.isPending}
            error={prepare.error}
            onSubmit={(password) => prepare.mutate(password)}
          />
        )}
      </div>
    </Empty>
  )
}
