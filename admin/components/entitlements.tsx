"use client"

import { useState } from "react"
import { ActionDialog } from "@/components/action-dialog"
import { SelectField, TextField } from "@/components/fields"
import { StatusBadge } from "@/components/filters"
import { Facts } from "@/components/page"
import { Time } from "@/components/time"
import { Button } from "@/components/ui/button"
import { operatorAction, useApiQuery, useOperator, type Data } from "@/lib/api"
import { formatBytes } from "@/lib/format"
import { useNow } from "@/lib/use-now"

type Entitlements = Data<"/v1/admin/billing/workspaces/{workspace_id}/entitlements">
type Grant = Entitlements["grants"][number]

/** What a limit is called in the console, and which workspaces it applies to. */
const kinds: Record<string, { label: string; team?: boolean }> = {
  machines: { label: "Machines" },
  publications: { label: "Previews & tunnels" },
  browser_terminals: { label: "Browser terminals" },
  public_tcp: { label: "Public TCP tunnels" },
  seats: { label: "Seats", team: true },
  viewers_per_terminal: { label: "Viewers per shared terminal" },
  sso: { label: "SSO add-on", team: true },
  scim: { label: "SCIM add-on", team: true },
}
/** The add-ons are on or off: their grant has no value (amount 1). */
const isAddon = (kind: string) => kind === "sso" || kind === "scim"
const label = (kind: string) => (kind === "bandwidth" ? "Bandwidth" : kind === "throttle_lift" ? "Throttle lift" : kinds[kind]?.label ?? kind)
/** 1234567 as "1,234,567" (not toLocaleString: its output depends on the viewer's locale). */
const count = (v: number) => String(v).replace(/\B(?=(\d{3})+(?!\d))/g, ",")
const amount = (g: Grant) => (g.kind === "bandwidth" ? formatBytes(g.amount) : g.kind === "throttle_lift" ? "Full speed" : isAddon(g.kind) ? "On" : count(g.amount))

/** An <input type=datetime-local> value to RFC 3339 UTC, "" when empty, undefined when it is not a future time. */
const endsAt = (local: string, now: number): string | undefined => {
  if (!local) return ""
  const parsed = Date.parse(local)
  return Number.isNaN(parsed) || parsed <= now ? undefined : new Date(parsed).toISOString()
}

/**
 * What a customer can use beyond (or below) their plan: the bandwidth allowance with what operators granted, every counted limit
 * with the plan's value and any override, and the grants themselves. Writes: grant bandwidth (GB, optional end), override a limit
 * (a value, optional end), lift the bandwidth throttle (full speed while it runs), revoke a grant. Enforcement reads the same grants, so
 * what is shown here is what is enforced.
 */
export function Entitlements({ workspaceId, scope }: { workspaceId: string; scope: "personal" | "team" }) {
  const admin = useOperator()?.role === "admin"
  const path = `/v1/admin/billing/workspaces/${encodeURIComponent(workspaceId)}/entitlements`
  const read = useApiQuery<Entitlements>(["entitlements", workspaceId], admin ? path : null)
  const [gb, setGb] = useState("100")
  const [kind, setKind] = useState("machines")
  const [value, setValue] = useState("")
  const [ends, setEnds] = useState("")
  const now = useNow()
  if (!admin || !read.data) return null
  const e = read.data
  const bytes = Number(gb)
  const gbOk = /^\d{1,7}(\.\d{1,3})?$/.test(gb) && bytes > 0
  const limitKinds = Object.entries(kinds).filter(([, k]) => !k.team || scope === "team")
  const valueOk = isAddon(kind) || (/^\d{1,9}$/.test(value) && Number(value) >= 1)
  const ending = endsAt(ends, now)
  const plan = e.bandwidth.plan_bytes
  const running = e.grants.filter((g) => g.running)
  const ended = e.grants.filter((g) => !g.running)
  return (
    <div className="flex flex-col gap-3" data-entitlements>
      <div className="flex flex-wrap items-center gap-2" data-entitlement-actions>
        <ActionDialog
          title={`Grant bandwidth to ${workspaceId}`}
          description="Extra hosted bandwidth, added to the allowance and to other running grants. It ends by itself at the date, or when revoked."
          target={workspaceId}
          submitLabel="Grant bandwidth"
          valid={gbOk && ending !== undefined}
          trigger={(open) => <Button variant="outline" size="lg" onClick={open} data-action="grant-bandwidth">Grant bandwidth</Button>}
          run={(h) => operatorAction("POST", path, h, { entitlement: "bandwidth", amount: Math.round(bytes * 1e9), ...(ending ? { ends_at: ending } : {}) })}
          onDone={() => setEnds("")}
        >
          <TextField label="Amount (GB)" value={gb} onChange={setGb} inputMode="decimal" hint="Decimal gigabytes: 100 = 100 GB, as the console counts them." />
          <TextField label="Ends" optional type="datetime-local" value={ends} onChange={setEnds} hint={ending === undefined ? "Pick a time in the future." : "Empty keeps it until you revoke it."} />
        </ActionDialog>
        {e.bandwidth.throttled && (
          <ActionDialog
            title={`Lift the throttle of ${workspaceId}`}
            description="Edge and relay traffic of this workspace runs at full speed until the date, or until you revoke the lift. It is recorded with your reference."
            target={workspaceId}
            submitLabel="Lift throttle"
            valid={ending !== undefined}
            trigger={(open) => <Button size="lg" onClick={open} data-action="lift-throttle">Lift throttle</Button>}
            run={(h) => operatorAction("POST", path, h, { entitlement: "throttle_lift", amount: 1, ...(ending ? { ends_at: ending } : {}) })}
            onDone={() => setEnds("")}
          >
            <TextField label="Ends" optional type="datetime-local" value={ends} onChange={setEnds} hint={ending === undefined ? "Pick a time in the future." : "Empty keeps it until you revoke it."} />
          </ActionDialog>
        )}
        <ActionDialog
          title={`Override a limit of ${workspaceId}`}
          description="Replaces the plan's value for this workspace, higher or lower, and a newer override of the same limit replaces this one. The SSO and SCIM add-ons are switched on instead, with no value."
          target={workspaceId}
          submitLabel="Set limit"
          valid={valueOk && ending !== undefined}
          trigger={(open) => <Button variant="outline" size="lg" onClick={open} data-action="override-limit">Override a limit</Button>}
          run={(h) => operatorAction("POST", path, h, { entitlement: kind, amount: isAddon(kind) ? 1 : Number(value), ...(ending ? { ends_at: ending } : {}) })}
          onDone={() => {
            setValue("")
            setEnds("")
          }}
        >
          <SelectField label="Limit" value={kind} onChange={setKind} options={limitKinds.map(([k, v]): [string, string] => [k, v.label])} />
          {!isAddon(kind) && <TextField label="Value" value={value} onChange={setValue} inputMode="numeric" hint="The new limit: at least 1." />}
          <TextField label="Ends" optional type="datetime-local" value={ends} onChange={setEnds} hint={ending === undefined ? "Pick a time in the future." : "Empty keeps it until you revoke it."} />
        </ActionDialog>
      </div>
      <Facts
        rows={[
          [
            "Bandwidth",
            <span key="b" className="tabular-nums" data-entitlement-bandwidth>
              {plan === null ? `${formatBytes(e.bandwidth.usage_bytes)} used · no limit` : `${formatBytes(e.bandwidth.usage_bytes)} of ${formatBytes(plan + e.bandwidth.granted_bytes)}`}
              {plan !== null && e.bandwidth.granted_bytes > 0 && <span className="text-muted-foreground"> · {formatBytes(e.bandwidth.granted_bytes)} granted</span>}
            </span>,
          ],
          [
            "Edge and relay speed",
            <span key="t" data-entitlement-throttle={e.bandwidth.throttled ? "throttled" : e.bandwidth.throttle_lifted ? "lifted" : "full"}>
              {e.bandwidth.throttled ? (
                <>Throttled{e.bandwidth.resets_at && <> until <Time iso={e.bandwidth.resets_at} /></>}</>
              ) : e.bandwidth.throttle_lifted ? (
                "Full speed · throttle lifted by an operator"
              ) : (
                "Full speed"
              )}
            </span>,
          ],
          ...e.limits.map((l): [string, React.ReactNode] => [
            label(l.kind),
            <span key={l.kind} className="tabular-nums" data-entitlement-limit={l.kind}>
              {count(l.used)} of {l.limit === null ? "no limit" : count(l.limit)}
              {l.override != null && <span className="text-muted-foreground"> · granted (plan {count(l.plan)})</span>}
            </span>,
          ]),
          ...(e.seats
            ? [["Seats", <span key="s" className="tabular-nums" data-entitlement-limit="seats">{e.seats.used} used of {e.seats.override ?? e.seats.bought}{e.seats.override != null && <span className="text-muted-foreground"> · granted (bought {e.seats.bought})</span>}</span>] as [string, React.ReactNode]]
            : []),
        ]}
      />
      {running.length + ended.length > 0 && (
        <ul className="border-t border-divider text-sm" data-grants>
          {[...running, ...ended].map((g) => (
            <li key={g.id} className="flex min-h-12 flex-wrap items-center gap-x-4 gap-y-1 border-b border-divider py-1" data-grant={g.id}>
              <span className="w-44 shrink-0">{label(g.kind)}</span>
              <span className="w-28 shrink-0 tabular-nums">{amount(g)}</span>
              <span className="min-w-0 flex-1 text-muted-foreground">
                {g.running ? (g.ends_at ? <>Ends <Time iso={g.ends_at} /></> : "Until revoked") : g.revoked_at ? <>Revoked <Time iso={g.revoked_at} /></> : g.ends_at ? <>Ended <Time iso={g.ends_at} /></> : null}
                <span> · {g.created_by}</span>
              </span>
              <StatusBadge state={g.running ? "active" : g.revoked_at ? "revoked" : "ended"} />
              {g.running && (
                <ActionDialog
                  title={`Revoke the ${label(g.kind)} grant`}
                  description={`Ends this ${label(g.kind).toLowerCase()} grant of ${amount(g)} now.`}
                  target={workspaceId}
                  destructive
                  submitLabel="Revoke grant"
                  trigger={(open) => <Button variant="outline" size="xs" onClick={open} data-action="revoke-entitlement">Revoke</Button>}
                  run={(h) => operatorAction("POST", `${path}/revoke`, h, { grant_id: g.id })}
                />
              )}
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}
