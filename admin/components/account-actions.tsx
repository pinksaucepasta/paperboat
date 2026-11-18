"use client"

import { ActionDialog } from "@/components/action-dialog"
import { Button } from "@/components/ui/button"
import { operatorAction, useOperator } from "@/lib/api"

/** Suspend or restore one account or team. The revision is the one just read, so a stale page is refused. */
function BanButtons({ kind, id, label, suspended, revision }: { kind: "accounts" | "teams"; id: string; label: string; suspended: boolean; revision: number }) {
  const noun = kind === "accounts" ? "account" : "team"
  const path = (a: string) => `/v1/admin/${kind}/${encodeURIComponent(id)}/${a}`
  return suspended ? (
    <ActionDialog
      title={`Restore ${label}`}
      description={`Lifts the suspension: the ${noun} can sign in and use Paperboat again.`}
      target={id}
      submitLabel={`Restore ${noun}`}
      trigger={(open) => <Button variant="outline" onClick={open} data-action="unban">Restore {noun}</Button>}
      run={(h) => operatorAction("POST", path("unban"), h, { revision })}
    />
  ) : (
    <ActionDialog
      title={`Suspend ${label}`}
      description={`Ends ${noun === "account" ? "this account's sessions and blocks sign-in" : "the team's access"} and billing activity until it is restored. Machines stay enrolled but lose access. Reversible.`}
      target={id}
      destructive
      confirmWord={id}
      submitLabel={`Suspend ${noun}`}
      trigger={(open) => <Button variant="destructive" onClick={open} data-action="ban">Suspend {noun}</Button>}
      run={(h) => operatorAction("POST", path("ban"), h, { revision })}
    />
  )
}

export function UserActions({ id, email, status, version }: { id: string; email: string; status: string; version: number }) {
  const admin = useOperator()?.role === "admin"
  if (!admin) return null
  return (
    <div className="flex flex-wrap gap-2" data-user-actions>
      <BanButtons kind="accounts" id={id} label={email} suspended={status === "suspended"} revision={version} />
      {status === "active" && (
        <ActionDialog
          title={`Sign ${email} out everywhere`}
          description="Ends every browser session, CLI login and token, API token and open SSH session of this account now. The account stays active: the customer can sign in again at once. Use it when credentials may have leaked; use Suspend to block the account."
          target={id}
          destructive
          confirmWord={id}
          submitLabel="Sign out everywhere"
          trigger={(open) => <Button variant="outline" onClick={open} data-action="revoke-sessions">Sign out everywhere</Button>}
          run={(h) => operatorAction("POST", `/v1/admin/accounts/${encodeURIComponent(id)}/sessions/revoke`, h, { revision: version })}
        />
      )}
      <ActionDialog
        title={`Rotate the SSH user CA of ${email}`}
        description="Advances the account's managed SSH certificate authority one step (a new CA becomes pending, then active, and the old one retires). Machines pick it up on their next sync."
        target={id}
        destructive
        confirmWord={id}
        submitLabel="Rotate SSH user CA"
        trigger={(open) => <Button variant="outline" onClick={open}>Rotate SSH user CA</Button>}
        run={(h) => operatorAction<{ id: string; state: string; public_key: string }[]>("POST", `/v1/admin/managed-ssh/accounts/${encodeURIComponent(id)}/rotate-user-ca`, h)}
        secrets={(r) => r.map((ca) => ({ label: `CA ${ca.id} (${ca.state})`, value: ca.public_key }))}
      />
    </div>
  )
}

export function TeamActions({ id, suspended, generation }: { id: string; suspended: boolean; generation: number }) {
  const admin = useOperator()?.role === "admin"
  if (!admin) return null
  return (
    <div className="flex flex-wrap gap-2" data-team-actions>
      <BanButtons kind="teams" id={id} label={id} suspended={suspended} revision={generation} />
    </div>
  )
}
