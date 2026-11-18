"use client"

import { useState } from "react"
import { ErrorBlock } from "@/components/error-block"
import { Mono, StatusBadge } from "@/components/filters"
import { Page, Panel, Stat } from "@/components/page"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { Time } from "@/components/time"
import { Skeleton } from "@/components/ui/skeleton"
import { ApiError, useApiQuery, type Data } from "@/lib/api"

type Overview = Data<"/v1/admin/overview">
type Analytics = Data<"/v1/admin/analytics/overview">
type Status = Data<"/v1/admin/status">

const sum = (rows: { count: number }[]) => rows.reduce((n, r) => n + r.count, 0)
const states = (rows: { state: string; count: number }[]) => rows.map((r) => `${r.count} ${r.state.replaceAll("_", " ")}`).join(" · ") || "none"

export default function OverviewPage() {
  const overview = useApiQuery<Overview>(["overview"], "/v1/admin/overview", 30_000)
  const status = useApiQuery<Status>(["status"], "/v1/admin/status", 30_000)
  return (
    <Page title="Overview">
      {overview.isError ? (
        <ErrorBlock what="the overview" error={overview.error} retry={() => overview.refetch()} />
      ) : (
        <OverviewStats data={overview.data} />
      )}
      <Panel title="Control plane">
        {status.isError ? (
          <ErrorBlock what="the server status" error={status.error} retry={() => status.refetch()} />
        ) : !status.data ? (
          <Skeleton className="h-24 w-full" />
        ) : (
          <div className="grid grid-cols-2 gap-x-6 text-sm sm:grid-cols-3 lg:grid-cols-5">
            {[["readiness", status.data.readiness === "ready" ? "ready" : "failed"] as const, ...Object.entries(status.data.dimensions).map(([name, d]) => [name, d.status === "healthy" || d.status === "ok" ? "ready" : (d.status ?? "unknown")] as const)].map(([name, state]) => (
              <div key={name} className="flex items-center justify-between gap-2 border-b border-divider py-2">
                <span className="truncate inline-block first-letter:uppercase">{name}</span>
                <StatusBadge state={state} />
              </div>
            ))}
          </div>
        )}
        {status.data && (
          <p className="text-xs text-muted-foreground" data-build>
            Build <Mono>{status.data.build.version}</Mono> <Mono>{status.data.build.commit.slice(0, 12)}</Mono> · observed <Time iso={status.data.observed_at} relative />
          </p>
        )}
      </Panel>
      <Insights />
    </Page>
  )
}

function OverviewStats({ data }: { data?: Overview }) {
  const v = (value: (d: Overview) => React.ReactNode) => (data ? value(data) : <Skeleton className="my-1.5 h-5 w-12" />)
  const n = (value: (d: Overview) => string) => (data ? value(data) : <Skeleton className="my-0.5 h-3 w-24" />)
  return (
    <div className="grid grid-cols-2 gap-3 md:grid-cols-3 lg:grid-cols-5" data-overview-ready={data ? "" : undefined}>
      <Stat label="Accounts" value={v((d) => sum(d.accounts))} note={n((d) => states(d.accounts))} />
      <Stat label="Machines" value={v((d) => d.hosts)} note={n((d) => `${d.hosts_ready} ready · ${d.hosts_stale} stale`)} />
      <Stat label="Relays" value={v((d) => d.relays)} note={n((d) => `${d.relays_ready} ready · ${d.relays_stale} stale`)} />
      <Stat label="Billing workspaces" value={v((d) => sum(d.billing_workspaces))} note={n((d) => states(d.billing_workspaces))} />
      <Stat label="Control operations" value={v((d) => sum(d.control_operations))} note={n((d) => states(d.control_operations))} />
    </div>
  )
}

function Insights() {
  const [days, setDays] = useState<"7" | "30" | "90">("30")
  const analytics = useApiQuery<Analytics>(["analytics", days], `/v1/admin/analytics/overview?days=${days}`)
  const notConfigured = analytics.error instanceof ApiError && analytics.error.code === "analytics_not_configured"
  return (
    <Panel title="Visitors" action={
        <ToggleGroup value={[days]} onValueChange={(next) => next[0] && setDays(next[0] as typeof days)} variant="outline" spacing={0}>
          {(["7", "30", "90"] as const).map((d) => (
            <ToggleGroupItem key={d} value={d}>
              {d} days
            </ToggleGroupItem>
          ))}
        </ToggleGroup>
      }>
      {notConfigured ? (
        <p className="py-12 text-center text-sm text-muted-foreground" data-analytics-state="not-configured">
          PostHog isn’t set up on this server (PAPERBOAT_POSTHOG_*).
        </p>
      ) : analytics.isError ? (
        <ErrorBlock what="analytics" error={analytics.error} retry={() => analytics.refetch()} />
      ) : !analytics.data ? (
        <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
          {["Site visits", "Page views", "Active console users", "Machines paired"].map((label) => (
            <Stat key={label} label={label} value={<Skeleton className="my-1.5 h-5 w-12" />} />
          ))}
        </div>
      ) : (
        <AnalyticsView data={analytics.data} />
      )}
    </Panel>
  )
}

/** posthogquery's errorCode values, in words; the code stays visible below for support. */
const insightFailure: Record<string, string> = {
  unauthorized: "PostHog refused the API key.",
  rate_limited: "PostHog is rate limiting; try again in a minute.",
  provider_unavailable: "PostHog is down; try again later.",
  rejected: "PostHog rejected the query.",
  timeout: "PostHog took too long; try again.",
  unexpected_response: "PostHog sent an answer the server couldn’t read.",
}

function AnalyticsView({ data }: { data: Analytics }) {
  const failed = Object.keys(data.errors ?? {})
  const peak = Math.max(1, ...data.daily.map((d) => d.page_views))
  return (
    <div className="flex flex-col gap-6" data-analytics-state="ready">
      {failed.map((k) => (
        <p key={k} role="alert" className="flex flex-col text-sm text-destructive" data-insight-error={k}>
          Couldn’t load {k}. {insightFailure[data.errors?.[k] ?? ""] ?? "Try again later."}
          <span className="text-xs text-muted-foreground">
            Code: <code className="font-mono">{data.errors?.[k]}</code>
          </span>
        </p>
      ))}
      <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
        <Stat label="Site visits" value={data.totals.visitors} />
        <Stat label="Page views" value={data.totals.page_views} />
        <Stat label="Active console users" value={data.totals.console_users} />
        <Stat label="Machines paired" value={data.totals.machines_paired} />
      </div>
      <div className="grid gap-6 md:grid-cols-2">
        <Panel title="Funnel">
          <ol className="text-sm">
            {data.funnel.map((s, i) => (
              <li key={s.step} className="flex items-center justify-between border-b border-divider py-2.5">
                <span>{s.step}</span>
                <span className="tabular-nums">{s.count}{i > 0 && data.funnel[i - 1].count > 0 ? <span className="ml-2 inline-block w-10 text-right text-xs text-muted-foreground">{Math.round((s.count / data.funnel[i - 1].count) * 100)}%</span> : null}</span>
              </li>
            ))}
          </ol>
        </Panel>
        <Panel title="Page views per day">
          <ul className="flex h-32 items-end gap-0.5 border-b border-divider" aria-label="Page views per day">
            {data.daily.map((d) => (
              <li key={d.day} title={`${d.day}: ${d.page_views} views, ${d.visitors} visits`} className="flex-1 rounded-t-sm bg-primary/70" style={{ height: `${Math.max(2, (d.page_views / peak) * 100)}%` }} />
            ))}
          </ul>
        </Panel>
      </div>
      <Retention cohorts={data.retention} />
      <div className="grid gap-6 md:grid-cols-2">
        <Breakdown title="Top pages" rows={data.top_pages} />
        <Breakdown title="Referrers" rows={data.referrers} />
        <Breakdown title="Countries" rows={data.countries} />
        <Breakdown title="Devices" rows={data.devices.filter((r) => r.name !== "(none)")} />
      </div>
      <p className="text-xs text-muted-foreground">Observed <Time iso={data.observed_at} relative /></p>
    </div>
  )
}

/** Weekly cohorts by first machine pairing: the share still active n weeks later. */
function Retention({ cohorts }: { cohorts: Analytics["retention"] }) {
  const any = cohorts.some((c) => c.active.length > 0)
  return (
    <Panel title="Retention">
      {!any ? (
        <p className="py-6 text-center text-sm text-muted-foreground" data-retention-state="not-configured">
          No data yet.
        </p>
      ) : (
        <div className="overflow-x-auto" data-retention-state="ready">
          <table className="w-full border-collapse text-sm">
            <thead>
              <tr className="border-b border-divider text-left text-xs text-muted-foreground">
                <th className="h-9 px-2 font-normal">Week</th>
                <th className="px-2 text-right font-normal">Accounts</th>
                {cohorts[0].active.map((_, week) => (
                  <th key={week} className="px-2 text-right font-normal">+{week}w</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {cohorts.map((c) => (
                <tr key={c.week} className="border-b border-divider">
                  <td className="h-10 px-2">{c.week.slice(0, 10)}</td>
                  <td className="px-2 text-right tabular-nums">{c.size}</td>
                  {c.active.map((n, week) => (
                    <td key={week} className="px-2 text-right tabular-nums">{c.size > 0 ? `${Math.round((n / c.size) * 100)}%` : "—"}</td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </Panel>
  )
}

function Breakdown({ title, rows }: { title: string; rows: { name: string; count: number }[] }) {
  return (
    <Panel title={title}>
      {rows.length === 0 ? (
        <p className="py-6 text-center text-sm text-muted-foreground">No data</p>
      ) : (
        <ul className="text-sm">
          {rows.map((r) => (
            <li key={r.name} className="flex items-center justify-between gap-3 border-b border-divider py-2.5">
              <span className="min-w-0 truncate">{r.name}</span>
              <span className="tabular-nums">{r.count}</span>
            </li>
          ))}
        </ul>
      )}
    </Panel>
  )
}
