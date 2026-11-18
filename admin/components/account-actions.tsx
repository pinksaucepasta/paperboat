"use client"

import { useState } from "react"
import { ActionDialog } from "@/components/action-dialog"
import { SelectField, TextField } from "@/components/fields"
import { Mono } from "@/components/filters"
import { DangerRow, DangerZone, dangerChip } from "@/components/page"
import { Button } from "@/components/ui/button"
import { operatorAction, useApiQuery, useOperator, type Data } from "@/lib/api"

const chip = (label: string, open: () => void, action?: string) => (
  <Button variant="secondary" size="lg" className={dangerChip} onClick={open} data-action={action}>
    {label}
  </Button>
)

type RecentIPs = Data<"/v1/admin/accounts/{account_id}/ips">

/** The addresses an account recently signed in from; each can be blocked along with the ban. */
function RecentAddressChoices({ id, chosen, onChange }: { id: string; chosen: string[]; onChange: (next: string[]) => void }) {
  const ips = useApiQuery<RecentIPs>(["account-ips", id], `/v1/admin/accounts/${encodeURIComponent(id)}/ips`)
  const list = ips.data?.ips ?? []
  if (list.length === 0) return <p className="text-sm text-muted-foreground">{ips.isLoading ? "Loading recent addresses…" : "No recent sign-in addresses are on record."}</p>
  return (
    <fieldset className="flex flex-col gap-1.5" data-ban-ips>
      <legend className="mb-1 text-sm font-medium">Also block their recent addresses</legend>
      {list.map((v) => (
        <label key={v.ip} className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={v.blocked || chosen.includes(v.ip)}
            disabled={v.blocked}
            onChange={(e) => onChange(e.target.checked ? [...chosen, v.ip] : chosen.filter((x) => x !== v.ip))}
          />
          <Mono>{v.ip}</Mono>
          <span className="text-muted-foreground">{v.blocked ? "already blocked" : `${v.count} sign-in${v.count === 1 ? "" : "s"}`}</span>
        </label>
      ))}
    </fieldset>
  )
}

const subscriptionOptions: [string, string][] = [
  ["keep", "Leave them as they are"],
  ["period_end", "End them at the end of the paid period"],
  ["immediate", "Cancel them now"],
]

/** Ban or unban one account or team. The revision is the one just read, so a stale page is refused. */
function BanRow({ kind, id, label, banned, reason, revision }: { kind: "accounts" | "teams"; id: string; label: string; banned: boolean; reason?: string; revision: number }) {
  const noun = kind === "accounts" ? "account" : "team"
  const path = (a: string) => `/v1/admin/${kind}/${encodeURIComponent(id)}/${a}`
  const [subscriptions, setSubscriptions] = useState("keep")
  const [ips, setIPs] = useState<string[]>([])
  const [range, setRange] = useState("")
  const rangeOk = range.length <= 64 && /^[0-9a-fA-F:.]*(\/\d{1,3})?$/.test(range)
  const reset = () => {
    setSubscriptions("keep")
    setIPs([])
    setRange("")
  }
  return banned ? (
    <DangerRow title={`Unban ${noun}`} description={reason ? `Banned (${reason}). Sign-in works again.` : "Sign-in works again."}>
      <ActionDialog
        title={`Unban ${label}`}
        description={`${noun === "account" ? "It can sign in again and the teams its ban suspended come back" : "It works again for its members"}. Its machines join again with a new \`pb login\`. Subscriptions that were cancelled stay cancelled, and blocked addresses stay blocked until you lift them.`}
        target={id}
        confirmWord={id}
        submitLabel={`Unban ${noun}`}
        trigger={(open) => chip(`Unban ${noun}`, open, "unban")}
        run={(h) => operatorAction("POST", path("unban"), h, { revision })}
      />
    </DangerRow>
  ) : (
    <DangerRow title={`Ban ${noun}`} description="Blocks sign-in and sign-up with the same identity, ends access and takes it offline. The data is kept. Reversible.">
      <ActionDialog
        title={`Ban ${label}`}
        description={
          noun === "account"
            ? "Ends every session, login and token, cuts its machines' network access, takes its previews and tunnels offline (the edge answers the unavailable page) and suspends the teams it owns. Nobody can sign in or sign up with the same WorkOS identity or e-mail. Its data is kept."
            : "Suspends the team: its members lose access, its previews and tunnels go offline (the edge answers the unavailable page). Its data is kept."
        }
        target={id}
        destructive
        confirmWord={id}
        submitLabel={`Ban ${noun}`}
        valid={rangeOk}
        trigger={(open) => chip(`Ban ${noun}`, open, "ban")}
        run={(h) =>
          operatorAction("POST", path("ban"), h, {
            revision,
            subscriptions,
            ...(noun === "account" && ips.length + (range ? 1 : 0) > 0 ? { block_ips: [...ips, ...(range ? [range] : [])] } : {}),
          })
        }
        onDone={reset}
      >
        <SelectField label="Paid subscriptions" value={subscriptions} onChange={setSubscriptions} options={subscriptionOptions} hint="Stripe is told after the ban is saved, and retried if it is down." />
        {noun === "account" && (
          <>
            <RecentAddressChoices id={id} chosen={ips} onChange={setIPs} />
            <TextField label="Also block a range" optional value={range} onChange={setRange} placeholder="203.0.113.0/24" hint="An address or CIDR. IPv4 up to /8, IPv6 up to /16." />
          </>
        )}
      </ActionDialog>
    </DangerRow>
  )
}

export function UserActions({ id, email, status, reason, version }: { id: string; email: string; status: string; reason?: string; version: number }) {
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
        <BanRow kind="accounts" id={id} label={email} banned={status === "suspended"} reason={reason} revision={version} />
      </DangerZone>
    </div>
  )
}

export function TeamActions({ id, suspended, reason, generation }: { id: string; suspended: boolean; reason?: string; generation: number }) {
  const admin = useOperator()?.role === "admin"
  if (!admin) return null
  return (
    <div data-team-actions>
      <DangerZone>
        <BanRow kind="teams" id={id} label={id} banned={suspended} reason={reason} revision={generation} />
      </DangerZone>
    </div>
  )
}
