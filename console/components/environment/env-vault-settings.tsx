"use client"

import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { useEffect, useState, useSyncExternalStore } from "react"
import { useApiQuery } from "@/lib/api"
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
import { ErrorMessage } from "@/components/error-reference"
import { useShellState } from "@/lib/use-shell-state"

type Change = "password" | "create" | "replace" | "off" | "key" | "reset"

/**
 * Environment page: the Vault section is always there. Unlocked it holds the password and recovery
 * code; before a vault exists or while it is locked it says what to do, with the recovery path in sight.
 */
export function EnvVaultSettings() {
  const unlocked = useSyncExternalStore(subscribeVaultLock, vaultUnlocked, () => false)
  const client = useQueryClient()
  const [change, setChange] = useShellState<Change>("environment/env-vault-settings.tsx:change")
  const close = () => setChange(undefined)
  const done = () => {
    close()
    return client.invalidateQueries({ queryKey: ["env-vault-recovery"] })
  }
  return (
    <>
      {unlocked ? <OpenVault onChange={setChange} /> : <ClosedVault />}
      {/* Here, not in OpenVault: a change that fails locks the vault, and its dialog must stay to say why. */}
      {change === "key" && (
        <ConfirmAction
          open
          onOpenChange={(open) => !open && close()}
          title="Replace your personal key?"
          description="Every variable is sealed again with a new key. This can take a moment."
          action="Replace key"
          pending="Replacing…"
          confirmName="ROTATE ENV"
          mutation={{
            mutationFn: rotatePersonalKey,
            onSuccess: () => client.invalidateQueries({ queryKey: ["env-names"] }),
          }}
        />
      )}
      {change === "reset" && <ResetDialog onClose={close} />}
      {change === "password" && <PasswordDialog onClose={close} onDone={done} />}
      {(change === "create" || change === "replace") && (
        <CodeDialog replacing={change === "replace"} onClose={close} onDone={done} />
      )}
      <TurnOff open={change === "off"} onClose={close} onDone={done} />
    </>
  )
}

function ClosedVault() {
  const vault = useApiQuery({ queryKey: ["env-vault"], queryFn: ({ signal }) => readVault(signal), staleTime: 0 })
  const [recovering, setRecovering] = useShellState("environment/env-vault-settings.tsx:recovering", false)
  const [resetting, setResetting] = useShellState("environment/env-vault-settings.tsx:resetting", false)
  const notSetUp = vault.isSuccess && !vault.data
  return (
    <Section title="Vault">
      <SettingRow
        label="Password"
        description={
          vault.isPending ? (
            <Skeleton className="my-0.5 h-4 w-56" />
          ) : vault.isError ? (
            <ErrorMessage error={vault.error} />
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

function OpenVault({ onChange }: { onChange: (change: Change) => void }) {
  const recovery = useQuery({ queryKey: ["env-vault-recovery"], queryFn: recoveryEnabled, staleTime: 0, gcTime: 0 })
  return (
    <Section title="Vault">
      <SettingRow label="Password">
        <Button variant="outline" onClick={() => onChange("password")}>
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
              <Button variant="outline" onClick={() => onChange("replace")}>
                Replace…
              </Button>
              <Button variant="outline" onClick={() => onChange("off")}>
                Turn off…
              </Button>
            </>
          ) : (
            <Button variant="outline" disabled={recovery.isPending} onClick={() => onChange("create")}>
              Create…
            </Button>
          )}
        </span>
      </SettingRow>
      <SettingRow label="Personal key" description="Seals your variables again with a new key. Your password stays.">
        <Button variant="outline" onClick={() => onChange("key")}>
          Replace…
        </Button>
      </SettingRow>
      <SettingRow
        label="Reset vault"
        description="Deletes your variables and team keys, then starts over with a new password."
      >
        <Button variant="outline" onClick={() => onChange("reset")}>
          Reset…
        </Button>
      </SettingRow>
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
          make.error && (
            <p className="text-sm text-destructive">
              <ErrorMessage error={make.error} />
            </p>
          )
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
        {off.isError && (
          <p className="text-sm text-destructive">
            <ErrorMessage error={off.error} />
          </p>
        )}
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
  const [typed, setTyped] = useShellState("environment/env-vault-settings.tsx:typed", "")
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
        <p className="text-sm text-muted-foreground">Your variables and team keys are deleted. This can’t be undone.</p>
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
            <Input
              id="reset-confirm"
              value={typed}
              autoComplete="off"
              onChange={(event) => setTyped(event.target.value)}
            />
          </Field>
        </PasswordForm>
      </DialogContent>
    </Dialog>
  )
}
