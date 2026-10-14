"use client"

import { EyeIcon, EyeSlashIcon, FileTextIcon } from "@phosphor-icons/react"
import { useMutation, useQueryClient } from "@tanstack/react-query"
import { useRef, useState } from "react"
import { toast } from "sonner"
import { useMachines, type Machine } from "@/lib/api"
import { namePattern, parseDotenv, saveVariables, unlockVault } from "@/lib/environment"
import { cn } from "@/lib/utils"
import { Choice } from "@/components/tunnels/share-file"
import { Button } from "@/components/ui/button"
import { Dialog, DialogContent, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldError, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Textarea } from "@/components/ui/textarea"

/** Where a change goes: every machine, or one machine as an override. */
export type Target = { scope: "global" } | { scope: "machine"; machineId: string }

/** A machine takes environment variables when its Paperboat service injects them. */
export function canInject(machine: Machine) {
  return Boolean(machine.capabilities?.environment_injection?.configured)
}

/** "Applies to: All machines ▾", picking the shared scope or one machine's overrides. */
function TargetChoice({ value, onChange }: { value: Target; onChange: (target: Target) => void }) {
  const machines = useMachines()
  const usable = (machines.data ?? []).filter(canInject)
  const label =
    value.scope === "global"
      ? "All machines"
      : `Only ${usable.find((entry) => entry.id === value.machineId)?.alias ?? "this machine"}`
  return (
    <div className="flex flex-wrap items-center gap-x-1.5 text-sm text-muted-foreground">
      <span>Applies to</span>
      <Choice
        label={label}
        value={value.scope === "global" ? "global" : value.machineId}
        options={[
          { value: "global", label: "All machines" },
          ...usable.map((entry) => ({ value: entry.id, label: `Only ${entry.alias}` })),
        ]}
        onChange={(next) => onChange(next === "global" ? { scope: "global" } : { scope: "machine", machineId: next })}
      />
    </div>
  )
}

/**
 * Unlocks the vault with its password, in this tab only. Values stay encrypted everywhere else;
 * the password never leaves the browser.
 */
export function UnlockDialog({ onClose, onUnlocked }: { onClose: () => void; onUnlocked?: () => void }) {
  const [password, setPassword] = useState("")
  const unlock = useMutation({
    mutationFn: () => unlockVault(password),
    onSuccess: () => {
      onClose()
      onUnlocked?.()
    },
    onError: () => setPassword(""),
  })
  return (
    <Dialog open onOpenChange={(next) => !next && !unlock.isPending && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Unlock environment</DialogTitle>
        </DialogHeader>
        <form
          className="flex flex-col gap-4"
          onSubmit={(event) => {
            event.preventDefault()
            unlock.mutate()
          }}
        >
          <Field data-invalid={unlock.isError} className="gap-1.5">
            <FieldLabel htmlFor="vault-password">Vault password</FieldLabel>
            <Input
              id="vault-password"
              type="password"
              autoFocus
              autoComplete="current-password"
              value={password}
              onChange={(event) => setPassword(event.target.value)}
              aria-invalid={unlock.isError}
            />
            {unlock.isError ? (
              <FieldError>{unlock.error.message}</FieldError>
            ) : (
              <p className="text-xs text-muted-foreground">
                Values are decrypted in this tab only, and locked again when you close it.
              </p>
            )}
          </Field>
          <DialogFooter>
            <Button type="button" variant="secondary" size="lg" onClick={onClose} disabled={unlock.isPending}>
              Cancel
            </Button>
            <Button type="submit" size="lg" disabled={!password || unlock.isPending}>
              {unlock.isPending ? "Unlocking…" : "Unlock"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

/** Add a variable, or edit one's value (the name and where it applies are fixed when editing). */
export function VariableDialog({
  target: initialTarget,
  name: existingName,
  value: existingValue,
  onClose,
}: {
  target: Target
  name?: string
  value?: string
  onClose: () => void
}) {
  const queryClient = useQueryClient()
  const [target, setTarget] = useState(initialTarget)
  const [name, setName] = useState(existingName ?? "")
  const [value, setValue] = useState(existingValue ?? "")
  const [shown, setShown] = useState(!existingName)
  const editing = existingName !== undefined
  const problem =
    name && !namePattern.test(name) ? "Letters, numbers and underscores, not starting with a number." : undefined
  const save = useMutation({
    mutationFn: () => saveVariables(target, [{ name, value }]),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["environment"] })
      toast.success(editing ? `${name} updated` : `${name} added`)
      onClose()
    },
  })
  return (
    <Dialog open onOpenChange={(next) => !next && !save.isPending && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>{editing ? `Edit ${existingName}` : "Add variable"}</DialogTitle>
        </DialogHeader>
        <form
          className="flex min-w-0 flex-col gap-4"
          onSubmit={(event) => {
            event.preventDefault()
            if (name && !problem) save.mutate()
          }}
        >
          {!editing && (
            <Field data-invalid={Boolean(problem)} className="gap-1.5">
              <FieldLabel htmlFor="variable-name">Name</FieldLabel>
              <Input
                id="variable-name"
                autoFocus
                placeholder="DATABASE_URL"
                value={name}
                onChange={(event) => setName(event.target.value.replace(/\s/g, "_"))}
                aria-invalid={Boolean(problem)}
                className="font-mono"
                autoComplete="off"
                spellCheck={false}
              />
              {problem && <FieldError>{problem}</FieldError>}
            </Field>
          )}
          <Field className="gap-1.5">
            <FieldLabel htmlFor="variable-value" className="flex items-baseline justify-between">
              Value
              <button
                type="button"
                onClick={() => setShown(!shown)}
                className="flex items-center gap-1 text-xs font-normal text-muted-foreground hover:text-foreground"
              >
                {shown ? <EyeSlashIcon className="size-3.5" /> : <EyeIcon className="size-3.5" />}
                {shown ? "Hide" : "Show"}
              </button>
            </FieldLabel>
            <Textarea
              id="variable-value"
              autoFocus={editing}
              rows={3}
              value={value}
              onChange={(event) => setValue(event.target.value)}
              className={cn("font-mono text-code", !shown && "[-webkit-text-security:disc]")}
              autoComplete="off"
              spellCheck={false}
            />
          </Field>
          {!editing && <TargetChoice value={target} onChange={setTarget} />}
          {save.isError && <p className="text-sm text-destructive">{save.error.message}</p>}
          <DialogFooter>
            <Button type="button" variant="secondary" size="lg" onClick={onClose} disabled={save.isPending}>
              Cancel
            </Button>
            <Button type="submit" size="lg" disabled={!name || Boolean(problem) || save.isPending}>
              {save.isPending ? "Saving…" : editing ? "Save" : "Add variable"}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

/** Paste or drop a .env file; every variable in it is saved in one step. */
export function ImportDialog({ target: initialTarget, onClose }: { target: Target; onClose: () => void }) {
  const queryClient = useQueryClient()
  const [target, setTarget] = useState(initialTarget)
  const [text, setText] = useState("")
  const [over, setOver] = useState(false)
  const inputRef = useRef<HTMLInputElement>(null)
  const entries = parseDotenv(text)
  const save = useMutation({
    mutationFn: () => saveVariables(target, entries),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["environment"] })
      toast.success(`${entries.length} ${entries.length === 1 ? "variable" : "variables"} imported`)
      onClose()
    },
  })
  const read = (file?: File) => void file?.text().then(setText)
  return (
    <Dialog open onOpenChange={(next) => !next && !save.isPending && onClose()}>
      <DialogContent className="gap-4 sm:max-w-md">
        <DialogHeader>
          <DialogTitle>Import .env</DialogTitle>
        </DialogHeader>
        <div className="flex min-w-0 flex-col gap-4">
          <Textarea
            data-file-drop
            autoFocus
            rows={7}
            placeholder={"DATABASE_URL=postgres://…\nAPI_KEY=…"}
            value={text}
            onChange={(event) => setText(event.target.value)}
            onDragOver={(event) => {
              event.preventDefault()
              setOver(true)
            }}
            onDragLeave={() => setOver(false)}
            onDrop={(event) => {
              event.preventDefault()
              setOver(false)
              read(event.dataTransfer.files[0])
            }}
            className={cn("font-mono text-code [-webkit-text-security:disc]", over && "border-ring bg-ring/5")}
            autoComplete="off"
            spellCheck={false}
          />
          <div className="flex items-center justify-between text-xs text-muted-foreground">
            <span>
              {entries.length
                ? `${entries.length} ${entries.length === 1 ? "variable" : "variables"}: ${entries
                    .slice(0, 4)
                    .map((entry) => entry.name)
                    .join(", ")}${entries.length > 4 ? "…" : ""}`
                : "Paste a .env file, or drop one here."}
            </span>
            <button
              type="button"
              onClick={() => inputRef.current?.click()}
              className="flex shrink-0 items-center gap-1 underline-offset-4 hover:text-foreground hover:underline"
            >
              <FileTextIcon className="size-3.5" />
              Choose file
            </button>
          </div>
          <TargetChoice value={target} onChange={setTarget} />
          {save.isError && <p className="text-sm text-destructive">{save.error.message}</p>}
          <input
            ref={inputRef}
            type="file"
            hidden
            onChange={(event) => {
              read(event.currentTarget.files?.[0])
              event.currentTarget.value = ""
            }}
          />
          <DialogFooter>
            <Button type="button" variant="secondary" size="lg" onClick={onClose} disabled={save.isPending}>
              Cancel
            </Button>
            <Button size="lg" onClick={() => save.mutate()} disabled={!entries.length || save.isPending}>
              {save.isPending ? "Importing…" : `Import ${entries.length || ""}`.trim()}
            </Button>
          </DialogFooter>
        </div>
      </DialogContent>
    </Dialog>
  )
}
