"use client"

import { useQueryClient } from "@tanstack/react-query"
import { CopyIcon } from "@phosphor-icons/react"
import { useId, useState } from "react"
import { CopyReference } from "@/components/copy-reference"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { ApiError, referencePattern, type ActionHeaders } from "@/lib/api"

/** A value the server shows once (a runtime credential, recovery codes): shown with a copy button, gated by an acknowledgement. */
export type OneTimeSecret = { label: string; value: string }

type Props<R> = {
  /** The button the operator presses to start the action; leave it out and pass `open` to open it from a menu. */
  trigger?: (open: () => void) => React.ReactNode
  open?: boolean
  onOpenChange?: (open: boolean) => void
  title: string
  /** What happens, to whom, and what it affects (shown before the form). */
  description: React.ReactNode
  /** The id the action acts on; sent as Operator-Target. */
  target: string
  /** Typed confirmation for destructive actions: the operator must type this exactly. */
  confirmWord?: string
  destructive?: boolean
  submitLabel: string
  /** The form fields (the page owns their state). */
  children?: React.ReactNode
  /** False keeps the submit button disabled (the page validates its fields). */
  valid?: boolean
  /** Performs the request with the headers the dialog collected. */
  run: (headers: ActionHeaders) => Promise<R>
  /** One-time secrets in the result, if any. */
  secrets?: (result: R) => OneTimeSecret[]
  /** Called after the action succeeded and the dialog closed. */
  onDone?: (result: R) => void
}

/**
 * Every operator write goes through here: a confirmation with what it does, the fields, an audit reference
 * (Operator-Reason, required), a typed confirmation when destructive, a fresh idempotency key that stays the same
 * across retries of one attempt, the failure with its support reference, and one-time secrets shown once.
 */
export function ActionDialog<R>(props: Props<R>) {
  const client = useQueryClient()
  const [own, setOwn] = useState(false)
  const open = props.open ?? own
  const setOpen = (next: boolean) => (props.onOpenChange ?? setOwn)(next)
  const [reason, setReason] = useState("")
  const [typed, setTyped] = useState("")
  const [key, setKey] = useState(() => crypto.randomUUID())
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<Error>()
  const [secrets, setSecrets] = useState<OneTimeSecret[]>()
  const [saved, setSaved] = useState(false)
  const [result, setResult] = useState<R>()
  const id = useId()

  const reset = () => {
    setReason("")
    setTyped("")
    setKey(crypto.randomUUID())
    setError(undefined)
    setSecrets(undefined)
    setSaved(false)
    setResult(undefined)
  }
  const close = () => {
    // Not while the request is out: closing would lose a one-time secret the answer is about to carry.
    if (busy || (secrets && !saved)) return
    setOpen(false)
    if (result !== undefined || secrets) props.onDone?.(result as R)
    reset()
  }
  const reasonOk = referencePattern.test(reason)
  const typedOk = !props.confirmWord || typed === props.confirmWord
  const canSubmit = reasonOk && typedOk && props.valid !== false && !busy

  async function submit(event: React.FormEvent) {
    event.preventDefault()
    setBusy(true)
    setError(undefined)
    try {
      const done = await props.run({ target: props.target, reason, key })
      // The next attempt is a new action: it gets its own key.
      setKey(crypto.randomUUID())
      void client.invalidateQueries()
      const shown = props.secrets?.(done) ?? []
      if (shown.length > 0) {
        setResult(done)
        setSecrets(shown)
      } else {
        setOpen(false)
        props.onDone?.(done)
        reset()
      }
    } catch (e) {
      // The key is kept: retrying an unknown outcome must not apply the action twice.
      setError(e as Error)
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      {props.trigger?.(() => setOpen(true))}
      <Dialog open={open} onOpenChange={(next) => (next ? setOpen(true) : close())}>
        <DialogContent className="max-h-[90svh] gap-4 overflow-y-auto sm:max-w-md" showCloseButton={!busy && (!secrets || saved)}>
          <DialogHeader>
            <DialogTitle>{secrets ? "Save this now" : props.title}</DialogTitle>
            <DialogDescription>{secrets ? "It won’t be shown again." : props.description}</DialogDescription>
          </DialogHeader>
          {secrets ? (
            <div className="flex flex-col gap-3" data-one-time-secrets>
              {secrets.map((s) => (
                <div key={s.label} className="flex flex-col gap-1.5">
                  <Label>{s.label}</Label>
                  <div className="surface flex items-start gap-2 p-3">
                    <code className="min-w-0 flex-1 break-all font-mono text-xs" data-secret>{s.value}</code>
                    <Button variant="ghost" size="icon-xs" aria-label={`Copy ${s.label}`} onClick={() => navigator.clipboard?.writeText(s.value).catch(() => {})}>
                      <CopyIcon />
                    </Button>
                  </div>
                </div>
              ))}
              <label className="flex items-center gap-2 text-sm">
                <Checkbox checked={saved} onCheckedChange={(v) => setSaved(v === true)} />
                I saved it
              </label>
              <DialogFooter>
                <Button size="lg" disabled={!saved} onClick={close}>Done</Button>
              </DialogFooter>
            </div>
          ) : (
            <form
              onSubmit={submit}
              // Changed input after a failure is a different action: it needs its own key (the server refuses one key for two inputs).
              onChange={() => error && setKey(crypto.randomUUID())}
              aria-busy={busy}
              className="flex flex-col gap-4"
              data-action-dialog={props.title}
            >
              {props.children}
              <div className="flex flex-col gap-1.5">
                <Label htmlFor={`${id}-reason`}>Reference</Label>
                <Input id={`${id}-reason`} aria-describedby={`${id}-reason-hint`} placeholder="Ticket or note id" value={reason} onChange={(e) => setReason(e.target.value.trim())} maxLength={128} required />
                <span id={`${id}-reason-hint`} className="text-xs text-muted-foreground">For the audit log. Letters, digits and _ . : -</span>
              </div>
              {props.confirmWord && (
                <div className="flex flex-col gap-1.5">
                  <Label htmlFor={`${id}-typed`}>Type <code className="font-mono">{props.confirmWord}</code> to confirm</Label>
                  <Input id={`${id}-typed`} autoComplete="off" value={typed} onChange={(e) => setTyped(e.target.value)} />
                </div>
              )}
              {error && (
                <div role="alert" className="flex flex-col gap-1 text-sm text-destructive" data-action-error>
                  <span>{error.message}</span>
                  {error instanceof ApiError && error.reference && <CopyReference reference={error.reference} />}
                  {error instanceof ApiError && error.status === 409 && <span className="text-xs text-muted-foreground">Reload and check the current state first.</span>}
                  {error instanceof ApiError && (error.status === 0 || error.status >= 500) && <span className="text-xs text-muted-foreground">Check the current state, then retry. A retry applies once.</span>}
                </div>
              )}
              <DialogFooter>
                <Button type="button" size="lg" variant="outline" disabled={busy} onClick={close}>Cancel</Button>
                <Button type="submit" size="lg" variant={props.destructive ? "destructive" : "default"} disabled={!canSubmit}>
                  {busy ? "Working…" : props.submitLabel}
                </Button>
              </DialogFooter>
            </form>
          )}
        </DialogContent>
      </Dialog>
    </>
  )
}
