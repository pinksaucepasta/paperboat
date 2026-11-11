"use client"

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useEffect, useState } from "react"
import { changePassword, newRecoveryCode, recoveryEnabled, replaceRecoveryCode } from "@/lib/envvault/protection"
import { PasswordForm, RecoveryCodeStep } from "@/components/environment/env-recovery-code"
import { Section, SettingRow } from "@/components/section"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogTitle } from "@/components/ui/dialog"

type Change = "password" | "create" | "replace" | "off"

/** Environment page, vault unlocked: the vault's password and its recovery code. */
export function EnvVaultSettings() {
  const client = useQueryClient()
  const [change, setChange] = useState<Change>()
  const recovery = useQuery({ queryKey: ["env-vault-recovery"], queryFn: recoveryEnabled, staleTime: 0, gcTime: 0 })
  const done = () => {
    setChange(undefined)
    return client.invalidateQueries({ queryKey: ["env-vault-recovery"] })
  }
  return (
    <Section title="Vault">
      <SettingRow label="Password">
        <Button variant="outline" onClick={() => setChange("password")}>
          Change…
        </Button>
      </SettingRow>
      <SettingRow
        label="Recovery code"
        description={
          recovery.isPending
            ? undefined
            : recovery.data
              ? "On"
              : "Off. If you forget your password, your variables can’t be unlocked."
        }
      >
        <span className="flex gap-2">
          {recovery.data ? (
            <>
              <Button variant="outline" onClick={() => setChange("replace")}>
                Replace…
              </Button>
              <Button variant="outline" onClick={() => setChange("off")}>
                Turn off…
              </Button>
            </>
          ) : (
            <Button variant="outline" disabled={recovery.isPending} onClick={() => setChange("create")}>
              Create…
            </Button>
          )}
        </span>
      </SettingRow>
      {change === "password" && <PasswordDialog onClose={() => setChange(undefined)} onDone={done} />}
      {(change === "create" || change === "replace") && (
        <CodeDialog replacing={change === "replace"} onClose={() => setChange(undefined)} onDone={done} />
      )}
      <TurnOff open={change === "off"} onClose={() => setChange(undefined)} onDone={done} />
    </Section>
  )
}

function PasswordDialog({ onClose, onDone }: { onClose: () => void; onDone: () => unknown }) {
  const change = useMutation({ mutationFn: changePassword, onSuccess: onDone })
  return (
    <Dialog open onOpenChange={(open) => !open && !change.isPending && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogTitle>Change password</DialogTitle>
        <PasswordForm
          submitLabel="Change password"
          pending={change.isPending}
          error={change.error}
          onSubmit={(p) => change.mutate(p)}
          onCancel={onClose}
        />
      </DialogContent>
    </Dialog>
  )
}

function CodeDialog({
  replacing,
  onClose,
  onDone,
}: {
  replacing: boolean
  onClose: () => void
  onDone: () => unknown
}) {
  const [code, setCode] = useState<string>()
  const save = useMutation({ mutationFn: () => replaceRecoveryCode(code!), onSuccess: onDone })
  const make = useMutation({ mutationFn: newRecoveryCode, onSuccess: setCode })
  useEffect(() => make.mutate(), []) // eslint-disable-line react-hooks/exhaustive-deps -- one new code per opening
  return (
    <Dialog open onOpenChange={(open) => !open && !save.isPending && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogTitle>{replacing ? "Replace recovery code" : "Create recovery code"}</DialogTitle>
        {code ? (
          <RecoveryCodeStep
            code={code}
            note={replacing ? "The old code no longer works." : undefined}
            confirmLabel={replacing ? "Replace" : "Create"}
            pending={save.isPending}
            error={save.error}
            onConfirm={() => save.mutate()}
            onCancel={onClose}
          />
        ) : (
          make.error && <p className="text-sm text-destructive">{make.error.message}</p>
        )}
      </DialogContent>
    </Dialog>
  )
}

function TurnOff({ open, onClose, onDone }: { open: boolean; onClose: () => void; onDone: () => unknown }) {
  const off = useMutation({ mutationFn: () => replaceRecoveryCode(""), onSuccess: onDone })
  return (
    <AlertDialog
      open={open}
      onOpenChange={(next) => {
        if (next || off.isPending) return
        off.reset()
        onClose()
      }}
    >
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>Turn off the recovery code?</AlertDialogTitle>
          <AlertDialogDescription>
            Only your password will unlock your variables. If you forget it, they can’t be recovered.
          </AlertDialogDescription>
        </AlertDialogHeader>
        {off.isError && <p className="text-sm text-destructive">{off.error.message}</p>}
        <AlertDialogFooter>
          <AlertDialogCancel disabled={off.isPending}>Cancel</AlertDialogCancel>
          <AlertDialogAction
            variant="destructive"
            disabled={off.isPending}
            onClick={(event) => {
              event.preventDefault()
              off.mutate()
            }}
          >
            {off.isPending ? "Turning off…" : "Turn off"}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  )
}
