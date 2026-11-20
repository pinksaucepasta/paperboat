"use client"

import { Panel } from "@/components/page"
import { ErrorBlock } from "@/components/error-block"
import { StatusBadge } from "@/components/filters"
import { Time } from "@/components/time"
import { Skeleton } from "@/components/ui/skeleton"
import { useApiQuery, type Data } from "@/lib/api"
import { cn } from "@/lib/utils"
import { openAlerts, pick, type CanaryPayload, type HostsPayload, type Level } from "@/lib/host-health"
import { useNow } from "@/lib/use-now"

const bar: Record<Level, string> = { normal: "bg-success", warning: "bg-amber-500", critical: "bg-destructive" }

/** Production host capacity (each host against its own capacity) and the production canary, as the canary runner last reported them. */
export function HostHealth() {
  const q = useApiQuery<Data<"/v1/admin/host-health">>(["admin-host-health"], "/v1/admin/host-health", 30_000)
  const now = useNow()
  if (q.error) return <ErrorBlock what="host health" error={q.error} retry={() => q.refetch()} />
  if (!q.data) return <Skeleton className="h-48 w-full" />
  const hosts = pick<HostsPayload>(q.data.reports, "hosts")
  const canary = pick<CanaryPayload>(q.data.reports, "canary")
  const alerts = openAlerts(q.data.reports, now)
  return (
    <div className="flex flex-col gap-8" data-host-health data-open-alerts={alerts.length}>
      <Panel title="Open alerts">
        {alerts.length === 0 ? (
          <p className="text-sm text-muted-foreground">No open alerts.</p>
        ) : (
          <ul className="divide-y divide-divider text-sm">
            {alerts.map((a) => (
              <li key={a.id} className="flex flex-col gap-0.5 py-2" data-alert={a.id}>
                <StatusBadge state={a.level === "critical" ? "failed" : "pending"} />
                <span className="font-medium">{a.title}</span>
                <span className="text-muted-foreground">{a.body}</span>
              </li>
            ))}
          </ul>
        )}
      </Panel>
      <Panel title="Host capacity" action={hosts && <span className="text-xs text-muted-foreground">Reported <Time iso={hosts.reported_at} relative /></span>}>
        {!hosts ? (
          <p className="text-sm text-muted-foreground">No host has reported yet.</p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full min-w-[32rem] border-collapse text-sm">
              <thead>
                <tr className="border-b border-divider text-left text-xs text-muted-foreground">
                  {["Host", "Resource", "Used", "Level"].map((h) => <th key={h} scope="col" className="h-9 px-3 font-normal">{h}</th>)}
                </tr>
              </thead>
              <tbody className="divide-y divide-divider">
                {Object.entries(hosts.payload.hosts).flatMap(([host, h]) =>
                  h.unreachable
                    ? [<tr key={host} className="[&>td]:h-10 [&>td]:px-3" data-host={host}><td className="font-medium">{host}</td><td colSpan={3}><StatusBadge state="failed" /> cannot be sampled</td></tr>]
                    : Object.entries(h.resources).map(([name, r]) => (
                        <tr key={`${host}:${name}`} className="[&>td]:h-10 [&>td]:px-3" data-host={host} data-resource={name} data-level={r.level}>
                          <td className="font-medium">{host}</td>
                          <td>{name}</td>
                          <td>
                            <div className="flex items-center gap-2">
                              <div className="h-1.5 w-28 rounded-full bg-muted" aria-hidden><div className={cn("h-full rounded-full", bar[r.level])} style={{ width: `${Math.min(100, r.percent)}%` }} /></div>
                              <span className="tabular-nums">{r.percent}%</span>
                            </div>
                          </td>
                          <td><span className="inline-flex items-center gap-1.5"><span className={cn("size-1.5 rounded-full", bar[r.level])} aria-hidden />{r.level}</span></td>
                        </tr>
                      )),
                )}
              </tbody>
            </table>
          </div>
        )}
      </Panel>
      <Panel title="Production canary" action={canary && <span className="text-xs text-muted-foreground">Reported <Time iso={canary.reported_at} relative /></span>}>
        {!canary ? (
          <p className="text-sm text-muted-foreground">The canary has not reported yet.</p>
        ) : (
          <div className="flex flex-col gap-2 text-sm" data-canary-red={canary.payload.red}>
            <StatusBadge state={canary.payload.red ? "failed" : "ready"} />
            {canary.payload.last && <p className="text-muted-foreground">Last run <Time iso={canary.payload.last.finished} relative />: {canary.payload.last.ok ? "all steps passed" : "failed"}.</p>}
            {Object.entries(canary.payload.failing).map(([step, runs]) => (
              <p key={step}><span className="font-medium">{step}</span> failed {runs} runs in a row{canary.payload.red_since && <> (red since <Time iso={canary.payload.red_since} relative />)</>}.</p>
            ))}
          </div>
        )}
      </Panel>
    </div>
  )
}
