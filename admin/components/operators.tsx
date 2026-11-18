"use client"

import { useState } from "react"
import { ActionDialog } from "@/components/action-dialog"
import { ErrorBlock } from "@/components/error-block"
import { RowMenu, type RowAction } from "@/components/row-menu"
import { SelectField, TextField } from "@/components/fields"
import { StatusBadge } from "@/components/filters"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { operatorAction, type Data } from "@/lib/api"
import { useTimeFormat } from "@/lib/use-time-format"
import { useAllPages } from "@/lib/paging"

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
      description="You get a setup token, shown once and valid for 72 hours. Send it to them privately."
      target="operators"
      submitLabel="Create invitation"
      valid={/^[^@\s]+@[^@\s]+$/.test(email.trim())}
      trigger={(open) => <Button size="lg" onClick={open} data-operator-action="create">Invite operator</Button>}
      run={(h) => operatorAction<Created>("POST", "/v1/admin/operators", h, { email: email.trim(), role })}
      secrets={(r) => (r.setup_token ? [{ label: `Setup token for ${email.trim()}`, value: r.setup_token }] : [])}
      onDone={() => setEmail("")}
    >
      <TextField label="Email" type="email" value={email} onChange={setEmail} autoComplete="off" />
      <SelectField label="Role" value={role} onChange={setRole} options={roles} />
    </ActionDialog>
  )
}

/** The session picker: every live session of the operator, or all of them. */
function SessionPicker({ id, value, onChange }: { id: string; value: string; onChange: (v: string) => void }) {
  const sessions = useAllPages<Result["sessions"][number]>(path(id, "sessions"), (page) => (page as unknown as Result).sessions)
  const time = useTimeFormat()
  if (sessions.isPending) return <Skeleton className="h-7 w-full" />
  if (sessions.isError) return <ErrorBlock what="the sessions" error={sessions.error} retry={() => void sessions.refetch()} />
  const rows = [...sessions.data.items].sort((a, b) => b.created_at.localeCompare(a.created_at))
  return (
    <div className="flex flex-col gap-1.5" data-operator-sessions>
      <SelectField
        label="Session"
        value={value}
        onChange={onChange}
        options={[["all", `All (${rows.length})`], ...rows.map((s): [string, string] => [s.id, `${s.mfa_pending ? "Signing in" : "Signed in"} · ${time.dateTime(s.created_at)}`])]}
      />
      {rows.length === 0 && <span className="text-xs text-muted-foreground">No live sessions.</span>}
      {sessions.data.capped && <span className="text-xs text-muted-foreground">Showing the first 1000 sessions; All ends every one.</span>}
    </div>
  )
}

export function OperatorRowActions({ op }: { op: OperatorRow }) {
  const [session, setSession] = useState("all")
  const disabled = op.status === "disabled"
  const actions: RowAction[] = [
    {
      key: "sessions",
      label: "End sessions",
      dialog: (c) => (
        <ActionDialog
          {...c}
          title={`Sessions of ${op.email}`}
          description="They can sign in again with their password and authenticator."
          target={op.id}
          submitLabel="End session"
          valid={op.live_sessions > 0}
          run={(h) => operatorAction("POST", path(op.id, "sessions/revoke"), h, { session_id: session })}
          onDone={() => setSession("all")}
        >
          {c.open && <SessionPicker id={op.id} value={session} onChange={setSession} />}
        </ActionDialog>
      ),
    },
    {
      key: "mfa-reset",
      label: "Reset MFA",
      dialog: (c) => (
        <ActionDialog
          {...c}
          title={`Reset the authenticator of ${op.email}`}
          description="Deletes their authenticator and recovery codes and ends their sessions. Confirm who they are first."
          target={op.id}
          destructive
          confirmWord={op.email}
          submitLabel="Reset authenticator"
          run={(h) => operatorAction("POST", path(op.id, "mfa-reset"), h, {})}
        />
      ),
    },
    disabled
      ? {
          key: "enable",
          label: "Enable",
          dialog: (c) => (
            <ActionDialog {...c} title={`Enable ${op.email}`} description="They can sign in again." target={op.id} submitLabel="Enable operator" run={(h) => operatorAction("POST", path(op.id, "enable"), h, {})} />
          ),
        }
      : {
          key: "disable",
          label: "Disable",
          destructive: true,
          dialog: (c) => (
            <ActionDialog
              {...c}
              title={`Disable ${op.email}`}
              description="Blocks sign-in and ends their sessions. Reversible."
              target={op.id}
              destructive
              confirmWord={op.email}
              submitLabel="Disable operator"
              run={(h) => operatorAction("POST", path(op.id, "disable"), h, {})}
            />
          ),
        },
  ]
  return <RowMenu label={op.email} actions={actions} data-operator-row={op.email} />
}

export function OperatorStatus({ op }: { op: OperatorRow }) {
  if (op.invited && op.status === "active") return <span className="inline-flex items-center gap-1.5 whitespace-nowrap" data-state-word="invited"><span className="size-1.5 rounded-full bg-muted-foreground/35" aria-hidden />Invited</span>
  return <StatusBadge state={op.status} />
}

export const operatorMfa = (op: OperatorRow) => (op.mfa_enrolled ? "Set" : op.invited ? "—" : "Not set up")
