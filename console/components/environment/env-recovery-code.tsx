"use client"

import { DownloadSimpleIcon } from "@phosphor-icons/react"
import { useState } from "react"
import { CommandBlock } from "@/components/add-machine-dialog"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Field, FieldError, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"

/** A recovery code is shown once: copy or download it, confirm it is saved, then the change is published. */
export function RecoveryCodeStep({
  code,
  note,
  confirmLabel,
  pending,
  error,
  onConfirm,
  onCancel,
}: {
  code: string
  /** What this code replaces, when something does. */
  note?: string
  confirmLabel: string
  pending?: boolean
  error?: Error | null
  onConfirm: () => void
  onCancel: () => void
}) {
  const [saved, setSaved] = useState(false)
  const download = () => {
    const url = URL.createObjectURL(new Blob([`${code}\n`], { type: "text/plain" }))
    const link = Object.assign(document.createElement("a"), { href: url, download: "paperboat-recovery-code.txt" })
    link.click()
    URL.revokeObjectURL(url)
  }
  return (
    <div className="flex flex-col gap-3 text-sm">
      <p className="text-muted-foreground">Shown once. Keep it somewhere safe.{note && ` ${note}`}</p>
      <CommandBlock command={code} label="Copy recovery code" wrap />
      <Button type="button" variant="outline" className="self-start" onClick={download}>
        <DownloadSimpleIcon data-icon="inline-start" /> Download
      </Button>
      <label className="flex cursor-pointer items-center gap-2.5">
        <Checkbox checked={saved} onCheckedChange={(on) => setSaved(Boolean(on))} />I saved my recovery code
      </label>
      {error && <FieldError>{error.message}</FieldError>}
      <div className="flex justify-end gap-2">
        <Button type="button" size="lg" variant="outline" disabled={pending} onClick={onCancel}>
          Cancel
        </Button>
        <Button type="button" size="lg" disabled={!saved || pending} onClick={onConfirm}>
          {pending ? "Saving…" : confirmLabel}
        </Button>
      </div>
    </div>
  )
}

/**
 * A new password typed twice. `children` are further fields above it; `ready` gates the submit for
 * them. The password is handed to `onSubmit` and cleared from the form at once.
 */
export function PasswordForm({
  children,
  submitLabel,
  pending,
  error,
  ready = true,
  onSubmit,
  onCancel,
}: {
  children?: React.ReactNode
  submitLabel: string
  pending?: boolean
  error?: Error | null
  ready?: boolean
  onSubmit: (password: string) => void
  onCancel?: () => void
}) {
  const [password, setPassword] = useState("")
  const [confirm, setConfirm] = useState("")
  const mismatch = confirm !== "" && confirm !== password
  return (
    <form
      className="flex flex-col gap-3 text-sm"
      onSubmit={(event) => {
        event.preventDefault()
        if (!password || mismatch || confirm !== password || !ready) return
        const secret = password
        setPassword("")
        setConfirm("")
        onSubmit(secret)
      }}
    >
      {children}
      <Field className="gap-1.5">
        <FieldLabel htmlFor="vault-new-password">New password</FieldLabel>
        <Input
          id="vault-new-password"
          type="password"
          name="vault-new-password"
          autoComplete="new-password"
          value={password}
          onChange={(event) => setPassword(event.target.value)}
        />
      </Field>
      <Field className="gap-1.5">
        <FieldLabel htmlFor="vault-confirm-password">Confirm password</FieldLabel>
        <Input
          id="vault-confirm-password"
          type="password"
          name="vault-confirm-password"
          autoComplete="new-password"
          value={confirm}
          onChange={(event) => setConfirm(event.target.value)}
          aria-invalid={mismatch}
        />
        {mismatch && <FieldError>The passwords don’t match.</FieldError>}
      </Field>
      {error && <FieldError>{error.message}</FieldError>}
      <div className="flex justify-end gap-2">
        {onCancel && (
          <Button type="button" size="lg" variant="outline" disabled={pending} onClick={onCancel}>
            Cancel
          </Button>
        )}
        <Button type="submit" size="lg" disabled={pending || !password || confirm !== password || !ready}>
          {pending ? "Working…" : submitLabel}
        </Button>
      </div>
    </form>
  )
}
