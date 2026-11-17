"use client"

import { useState } from "react"
import { ActionDialog } from "@/components/action-dialog"
import { ErrorBlock } from "@/components/error-block"
import { SelectField, TextField } from "@/components/fields"
import { StatusBadge } from "@/components/filters"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { operatorAction, useApiQuery, type Data } from "@/lib/api"
import { useTimeFormat } from "@/lib/use-time-format"

export type OperatorRow = Data<"/v1/admin/operators">["operators"][number]
type Result = Data<"/v1/admin/operators/{operator_id}/sessions">
type Created = { resource_id: string; setup_token?: string }

const roles: [string, string][] = [["support", "Support"], ["billing", "Billing"], ["operations", "Operations"], ["admin", "Admin"]]
const path = (id: string, action: string) => `/v1/admin/operators/${encodeURIComponent(id)}/${action}`

/** Invite an operator: the server returns a one-time setup token, shown once for the admin to hand over out of band. */
export function CreateOperator() {
  const [email, setEmail] = useState("")
  const [role, setRole] = useState("support")
  return (
    <ActionDialog<Created>
      title="Invite an operator"
      description="Creates the operator with no password and shows a setup token once, valid for 72 hours. Send it to them in person or over a private channel. They open this panel, enter their email, then the token, choose a password and set up an authenticator."
      target="operators"
      submitLabel="Create invitation"
      valid={/^[^@\s]+@[^@\s]+$/.test(email.trim())}
      trigger={(open) => <Button onClick={open} data-operator-action="create">Invite an operator</Button>}
      run={(h) => operatorAction<Created>("POST", "/v1/admin/operators", h, { email: email.trim(), role })}
      secrets={(r) => (r.setup_token ? [{ label: `Setup token for ${email.trim()}`, value: r.setup_token }] : [])}
      onDone={() => setEmail("")}
    >
      <TextField label="Email" type="email" value={email} onChange={setEmail} autoComplete="off" hint="Inviting an email whose setup is not finished replaces the old token." />
      <SelectField label="Role" value={role} onChange={setRole} options={roles} hint="Admins can manage operators, accounts, billing policy and infrastructure." />
    </ActionDialog>
  )
}

/** The session picker: every live session of the operator, or all of them. */
function SessionPicker({ id, value, onChange }: { id: string; value: string; onChange: (v: string) => void }) {
  const sessions = useApiQuery<Result>(["operator-sessions", id], path(id, "sessions"))
  const time = useTimeFormat()
  if (sessions.isPending) return <Skeleton className="h-7 w-full" />
  if (sessions.isError) return <ErrorBlock what="the sessions" error={sessions.error} retry={() => void sessions.refetch()} />
  const rows = sessions.data.sessions
  return (
    <div className="flex flex-col gap-1.5" data-operator-sessions>
      <SelectField
        label="Session"
        value={value}
        onChange={onChange}
        options={[["all", `All live sessions (${rows.length})`], ...rows.map((s): [string, string] => [s.id, `${s.mfa_pending ? "Signing in" : "Signed in"} · started ${time.dateTime(s.created_at)} · ends ${time.dateTime(s.expires_at)}`])]}
      />
      {rows.length === 0 && <span className="text-xs text-muted-foreground">No live sessions.</span>}
    </div>
  )
}

export function OperatorRowActions({ op }: { op: OperatorRow }) {
  const [session, setSession] = useState("all")
  const disabled = op.status === "disabled"
  return (
    <div className="flex flex-wrap gap-1.5" data-operator-row={op.email}>
      <ActionDialog
        title={`Sessions of ${op.email}`}
        description="Ends one or all of this operator's live sessions. They can sign in again with their password and authenticator."
        target={op.id}
        submitLabel="End session"
        valid={op.live_sessions > 0}
        trigger={(open) => <Button size="xs" variant="outline" onClick={open} data-operator-action="sessions">Sessions ({op.live_sessions})</Button>}
        run={(h) => operatorAction("POST", path(op.id, "sessions/revoke"), h, { session_id: session })}
        onDone={() => setSession("all")}
      >
        <SessionPicker id={op.id} value={session} onChange={setSession} />
      </ActionDialog>
      <ActionDialog
        title={`Reset the authenticator of ${op.email}`}
        description="Deletes their authenticator and recovery codes and ends all their sessions. At their next sign-in they set up a new authenticator with just their password, so confirm the person out of band first."
        target={op.id}
        destructive
        confirmWord={op.email}
        submitLabel="Reset authenticator"
        trigger={(open) => <Button size="xs" variant="outline" onClick={open} data-operator-action="mfa-reset">Reset MFA</Button>}
        run={(h) => operatorAction("POST", path(op.id, "mfa-reset"), h, {})}
      />
      {disabled ? (
        <ActionDialog
          title={`Enable ${op.email}`}
          description="They can sign in again. Their earlier sessions stay ended."
          target={op.id}
          submitLabel="Enable operator"
          trigger={(open) => <Button size="xs" variant="outline" onClick={open} data-operator-action="enable">Enable</Button>}
          run={(h) => operatorAction("POST", path(op.id, "enable"), h, {})}
        />
      ) : (
        <ActionDialog
          title={`Disable ${op.email}`}
          description="Blocks every sign-in and ends all their sessions now. Reversible. The last active admin cannot be disabled."
          target={op.id}
          destructive
          confirmWord={op.email}
          submitLabel="Disable operator"
          trigger={(open) => <Button size="xs" variant="destructive" onClick={open} data-operator-action="disable">Disable</Button>}
          run={(h) => operatorAction("POST", path(op.id, "disable"), h, {})}
        />
      )}
    </div>
  )
}

export function OperatorStatus({ op }: { op: OperatorRow }) {
  if (op.invited && op.status === "active") return <span className="text-xs text-muted-foreground">Invited, not set up</span>
  return <StatusBadge state={op.status} />
}

export const operatorMfa = (op: OperatorRow) => (op.mfa_enrolled ? "Authenticator set" : op.invited ? "—" : "Not set up")
