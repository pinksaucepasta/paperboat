"use client"

import { ActionDialog } from "@/components/action-dialog"
import { DangerRow, DangerZone, dangerChip } from "@/components/page"
import { Button } from "@/components/ui/button"
import { operatorAction, useOperator } from "@/lib/api"

const chip = (label: string, open: () => void, action?: string) => (
  <Button variant="secondary" size="lg" className={dangerChip} onClick={open} data-action={action}>
    {label}
  </Button>
)

/** Suspend or restore one account or team. The revision is the one just read, so a stale page is refused. */
function BanRow({ kind, id, label, suspended, revision }: { kind: "accounts" | "teams"; id: string; label: string; suspended: boolean; revision: number }) {
  const noun = kind === "accounts" ? "account" : "team"
  const path = (a: string) => `/v1/admin/${kind}/${encodeURIComponent(id)}/${a}`
  return suspended ? (
    <DangerRow title={`Restore ${noun}`} description="It can sign in and use Paperboat again.">
      <ActionDialog
        title={`Restore ${label}`}
        description="It can sign in and use Paperboat again."
        target={id}
        submitLabel={`Restore ${noun}`}
        trigger={(open) => chip(`Restore ${noun}`, open, "unban")}
        run={(h) => operatorAction("POST", path("unban"), h, { revision })}
      />
    </DangerRow>
  ) : (
    <DangerRow title={`Suspend ${noun}`} description="Blocks sign-in and billing. Machines lose access. Reversible.">
      <ActionDialog
        title={`Suspend ${label}`}
        description="Blocks sign-in and billing until restored. Machines stay enrolled but lose access."
        target={id}
        destructive
        confirmWord={id}
        submitLabel={`Suspend ${noun}`}
        trigger={(open) => chip(`Suspend ${noun}`, open, "ban")}
        run={(h) => operatorAction("POST", path("ban"), h, { revision })}
      />
    </DangerRow>
  )
}

export function UserActions({ id, email, status, version }: { id: string; email: string; status: string; version: number }) {
  const admin = useOperator()?.role === "admin"
  if (!admin) return null
  return (
    <div data-user-actions>
      <DangerZone>
        {status === "active" && (
          <DangerRow title="Sign out everywhere" description="Ends every session, CLI login, token and SSH session. The account stays usable.">
            <ActionDialog
              title={`Sign ${email} out everywhere`}
              description="Ends every browser session, CLI login, API token, recipe and SSH session now. They can sign in again at once."
              target={id}
              destructive
              confirmWord={id}
              submitLabel="Sign out everywhere"
              trigger={(open) => chip("Sign out everywhere", open, "revoke-sessions")}
              run={(h) => operatorAction("POST", `/v1/admin/accounts/${encodeURIComponent(id)}/sessions/revoke`, h, { revision: version })}
            />
          </DangerRow>
        )}
        <DangerRow title="Rotate SSH user CA" description="Machines pick up the new CA on their next sync.">
          <ActionDialog
            title={`Rotate the SSH user CA of ${email}`}
            description="A new CA becomes pending, then active; the old one retires. Machines pick it up on their next sync."
            target={id}
            destructive
            confirmWord={id}
            submitLabel="Rotate SSH user CA"
            trigger={(open) => chip("Rotate SSH user CA", open)}
            run={(h) => operatorAction<{ id: string; state: string; public_key: string }[]>("POST", `/v1/admin/managed-ssh/accounts/${encodeURIComponent(id)}/rotate-user-ca`, h)}
            secrets={(r) => r.map((ca) => ({ label: `CA ${ca.id} (${ca.state})`, value: ca.public_key }))}
          />
        </DangerRow>
        <BanRow kind="accounts" id={id} label={email} suspended={status === "suspended"} revision={version} />
      </DangerZone>
    </div>
  )
}

export function TeamActions({ id, suspended, generation }: { id: string; suspended: boolean; generation: number }) {
  const admin = useOperator()?.role === "admin"
  if (!admin) return null
  return (
    <div data-team-actions>
      <DangerZone>
        <BanRow kind="teams" id={id} label={id} suspended={suspended} revision={generation} />
      </DangerZone>
    </div>
  )
}
