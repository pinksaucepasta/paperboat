"use client"

import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useState } from "react"
import { lockVault } from "@/lib/envvault/crypto"
import { newRecoveryCode, recoverVault } from "@/lib/envvault/protection"
import { unlockVault } from "@/lib/envvault/vault"
import { PasswordForm, RecoveryCodeStep } from "@/components/environment/env-recovery-code"
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"

/**
 * Forgot the vault password: the recovery code opens the vault in this browser, a new password and a
 * new recovery code replace both, and the old code stops working. Nothing is published until the new
 * code is saved.
 */
export function EnvRecover({ onCancel }: { onCancel: () => void }) {
  const client = useQueryClient()
  const [code, setCode] = useState("")
  const [next, setNext] = useState<{ password: string; code: string }>()
  const cancel = () => {
    lockVault() // drops the keys the recovery code opened
    onCancel()
  }
  const check = useMutation({
    mutationFn: async (password: string) => {
      await unlockVault(code, "recovery")
      return { password, code: await newRecoveryCode() }
    },
    onSuccess: (checked) => {
      setCode("")
      setNext(checked)
    },
  })
  const finish = useMutation({
    mutationFn: () => recoverVault(next!.password, next!.code),
    onSuccess: () =>
      Promise.all([
        client.invalidateQueries({ queryKey: ["env-names"] }),
        client.invalidateQueries({ queryKey: ["env-vault"] }),
      ]),
  })
  return (
    <div className="flex flex-col gap-3 surface rounded-lg p-4 text-sm">
      <span className="font-medium">{next ? "Your new recovery code" : "Unlock with your recovery code"}</span>
      {next ? (
        <RecoveryCodeStep
          code={next.code}
          note="The old code no longer works."
          confirmLabel="Finish"
          pending={finish.isPending}
          error={finish.error}
          onConfirm={() => finish.mutate()}
          onCancel={cancel}
        />
      ) : (
        <PasswordForm
          submitLabel="Continue"
          pending={check.isPending}
          error={check.error}
          ready={code.trim() !== ""}
          onSubmit={(password) => check.mutate(password)}
          onCancel={cancel}
        >
          <Field className="gap-1.5">
            <FieldLabel htmlFor="vault-recovery-code">Recovery code</FieldLabel>
            <Input
              id="vault-recovery-code"
              name="vault-recovery-code"
              autoComplete="off"
              spellCheck={false}
              className="font-mono"
              value={code}
              onChange={(event) => setCode(event.target.value)}
            />
          </Field>
        </PasswordForm>
      )}
    </div>
  )
}
