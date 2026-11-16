"use client"

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useEffect, useState, useSyncExternalStore } from "react"
import { subscribeVaultLock, vaultUnlocked } from "@/lib/envvault/crypto"
import { readVault } from "@/lib/envvault/vault"
import { rotatePersonalKey, resetVault } from "@/lib/envvault/manage"
import { changePassword, newRecoveryCode, recoveryEnabled, replaceRecoveryCode } from "@/lib/envvault/protection"
import { EnvRecover } from "@/components/environment/env-recover"
import { PasswordForm, RecoveryCodeStep } from "@/components/environment/env-recovery-code"
import { ConfirmAction } from "@/components/list-toolbar"
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
import { Field, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"

type Change = "password" | "create" | "replace" | "off" | "key" | "reset"

/**
 * Environment page: the Vault section is always there. Unlocked it holds the password and recovery
 * code; before a vault exists or while it is locked it says what to do, with the recovery path in sight.
 */
export function EnvVaultSettings() {
  const unlocked = useSyncExternalStore(subscribeVaultLock, vaultUnlocked, () => false)
  return unlocked ? <OpenVault /> : <ClosedVault />
}

function ClosedVault() {
  const vault = useQuery({ queryKey: ["env-vault"], queryFn: ({ signal }) => readVault(signal), staleTime: 0 })
  const [recovering, setRecovering] = useState(false)
  const [resetting, setResetting] = useState(false)
  const notSetUp = vault.isSuccess && !vault.data
  return (
    <Section title="Vault">
      <SettingRow
        label="Password"
        description={
          vault.isPending ? (
            <Skeleton className="my-0.5 h-4 w-56" />
          ) : vault.isError ? (
            vault.error.message
          ) : notSetUp ? (
            "Not set up. Create your vault above to store variables."
          ) : (
            "Locked. Unlock it above to change the password or recovery code."
          )
        }
      >
        {!notSetUp && (
          <Button variant="link" className="h-auto p-0 text-muted-foreground" onClick={() => setRecovering(true)}>
            Forgot password? Use your recovery code
          </Button>
        )}
      </SettingRow>
      {!notSetUp && vault.isSuccess && (
        <SettingRow label="Lost both?" description="Starting over deletes your variables and your team keys.">
          <Button variant="outline" onClick={() => setResetting(true)}>
            Reset vault…
          </Button>
        </SettingRow>
      )}
      {recovering && <EnvRecover onCancel={() => setRecovering(false)} />}
      {resetting && <ResetDialog onClose={() => setResetting(false)} />}
    </Section>
  )
}

function OpenVault() {
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
      <SettingRow label="Personal key" description="Seals your variables again with a new key. Your password stays.">
        <Button variant="outline" onClick={() => setChange("key")}>
          Replace…
        </Button>
      </SettingRow>
      <SettingRow label="Reset vault" description="Deletes your variables and team keys, then starts over with a new password.">
        <Button variant="outline" onClick={() => setChange("reset")}>
          Reset…
        </Button>
      </SettingRow>
      {change === "key" && (
        <ConfirmAction
          open
          onOpenChange={(open) => !open && setChange(undefined)}
          title="Replace your personal key?"
          description="Every variable is sealed again with a new key. This can take a moment."
          action="Replace key"
          pending="Replacing…"
          confirmName="ROTATE ENV"
          mutation={{ mutationFn: rotatePersonalKey, onSuccess: () => client.invalidateQueries({ queryKey: ["env-names"] }) }}
        />
      )}
      {change === "reset" && <ResetDialog onClose={() => setChange(undefined)} />}
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

/** A new password for a vault whose old one is lost: every personal variable and team key goes. */
function ResetDialog({ onClose }: { onClose: () => void }) {
  const client = useQueryClient()
  const [typed, setTyped] = useState("")
  const reset = useMutation({
    mutationFn: resetVault,
    onSuccess: async () => {
      await client.invalidateQueries({ queryKey: ["env-names"] })
      await client.invalidateQueries({ queryKey: ["env-vault"] })
      await client.invalidateQueries({ queryKey: ["env-team"] })
      onClose()
    },
  })
  return (
    <Dialog open onOpenChange={(open) => !open && !reset.isPending && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogTitle>Reset your vault</DialogTitle>
        <p className="text-sm text-muted-foreground">
          Your variables and team keys are deleted. This can’t be undone.
        </p>
        <PasswordForm
          submitLabel="Reset vault"
          pending={reset.isPending}
          error={reset.error}
          ready={typed.trim() === "RESET ENV"}
          onSubmit={(password) => reset.mutate(password)}
          onCancel={onClose}
        >
          <Field className="gap-1.5">
            <FieldLabel htmlFor="reset-confirm" className="font-normal text-muted-foreground">
              Type <span className="font-mono font-medium text-foreground">RESET ENV</span> to confirm
            </FieldLabel>
            <Input id="reset-confirm" value={typed} autoComplete="off" onChange={(event) => setTyped(event.target.value)} />
          </Field>
        </PasswordForm>
      </DialogContent>
    </Dialog>
  )
}
