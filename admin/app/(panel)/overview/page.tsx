"use client"

import { useState } from "react"
import { ErrorBlock } from "@/components/error-block"
import { Mono, StatusBadge } from "@/components/filters"
import { Page, Panel, Stat } from "@/components/page"
import { Tabs } from "@/components/tabs"
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
    <Page title="Overview" description="Right now: accounts, machines, relays and the control plane.">
      {overview.isError ? (
        <ErrorBlock what="the overview" error={overview.error} retry={() => overview.refetch()} />
      ) : !overview.data ? (
        <div className="grid grid-cols-2 gap-3 md:grid-cols-4">{Array.from({ length: 4 }, (_, i) => <Skeleton key={i} className="h-24" />)}</div>
      ) : (
        <div className="grid grid-cols-2 gap-3 md:grid-cols-4" data-overview-ready>
          <Stat label="Accounts" value={sum(overview.data.accounts)} note={states(overview.data.accounts)} />
          <Stat label="Machines" value={overview.data.hosts} note={`${overview.data.hosts_ready} ready · ${overview.data.hosts_stale} stale`} />
          <Stat label="Relays" value={overview.data.relays} note={`${overview.data.relays_ready} ready · ${overview.data.relays_stale} stale`} />
          <Stat label="Billing workspaces" value={sum(overview.data.billing_workspaces)} note={states(overview.data.billing_workspaces)} />
          <Stat label="Control operations" value={sum(overview.data.control_operations)} note={states(overview.data.control_operations)} />
        </div>
      )}
      <Panel title="Control plane">
        {status.isError ? (
          <ErrorBlock what="the server status" error={status.error} retry={() => status.refetch()} />
        ) : !status.data ? (
          <Skeleton className="h-16 w-full" />
        ) : (
          <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-sm">
            <span>Readiness <StatusBadge state={status.data.readiness === "ready" ? "ready" : "failed"} /></span>
            {Object.entries(status.data.dimensions).map(([name, d]) => (
              <span key={name} className="flex items-center gap-1"><Mono>{name}</Mono> <StatusBadge state={d.status === "healthy" || d.status === "ok" ? "ready" : (d.status ?? "unknown")} /></span>
            ))}
            <span className="text-muted-foreground">Observed <Time iso={status.data.observed_at} relative /></span>
          </div>
        )}
        {status.data && (
          <p className="text-sm text-muted-foreground" data-build>
            Server build <Mono>{status.data.build.version}</Mono> <Mono>{status.data.build.commit.slice(0, 12)}</Mono>. Runtime nodes report a protocol version, not a software version: see Relays &amp; edges. Deployment history is not available yet.
          </p>
        )}
      </Panel>
      <Insights />
    </Page>
  )
}

function Insights() {
  const [days, setDays] = useState<"7" | "30" | "90">("30")
  const analytics = useApiQuery<Analytics>(["analytics", days], `/v1/admin/analytics/overview?days=${days}`)
  const notConfigured = analytics.error instanceof ApiError && analytics.error.code === "analytics_not_configured"
  return (
    <Panel title="Visitors and activation (PostHog)" action={<Tabs value={days} onChange={setDays} options={[["7", "7 days"], ["30", "30 days"], ["90", "90 days"]]} />}>
      {notConfigured ? (
        <p className="text-sm text-muted-foreground" data-analytics-state="not-configured">
          Product analytics is not configured on this server. Set PAPERBOAT_POSTHOG_HOST, PAPERBOAT_POSTHOG_PROJECT_ID and PAPERBOAT_POSTHOG_PERSONAL_API_KEY (or its _FILE) to show visitors, funnel, top pages, referrers, countries and devices.
        </p>
      ) : analytics.isError ? (
        <ErrorBlock what="analytics" error={analytics.error} retry={() => analytics.refetch()} />
      ) : !analytics.data ? (
        <Skeleton className="h-48 w-full" />
      ) : (
        <AnalyticsView data={analytics.data} />
      )}
    </Panel>
  )
}

function AnalyticsView({ data }: { data: Analytics }) {
  const failed = Object.keys(data.errors ?? {})
  const peak = Math.max(1, ...data.daily.map((d) => d.page_views))
  return (
    <div className="flex flex-col gap-6" data-analytics-state="ready">
      {failed.length > 0 && <p role="alert" className="text-sm text-destructive">Some insights did not load: {failed.map((k) => `${k} (${data.errors?.[k]})`).join(", ")}.</p>}
      <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
        <Stat label="Site visits" value={data.totals.visitors} note="Browser tabs on the marketing site" />
        <Stat label="Page views" value={data.totals.page_views} />
        <Stat label="Console users" value={data.totals.console_users} note="Accounts that did something" />
        <Stat label="Machines paired" value={data.totals.machines_paired} note="Activation: accounts with a first machine" />
      </div>
      <div className="grid gap-6 md:grid-cols-2">
        <Panel title="Funnel">
          <ol className="border-t border-divider text-sm">
            {data.funnel.map((s, i) => (
              <li key={s.step} className="flex items-center justify-between border-b border-divider py-2">
                <span>{s.step}</span>
                <span className="tabular-nums">{s.count}{i > 0 && data.funnel[i - 1].count > 0 ? <span className="ml-2 text-xs text-muted-foreground">{Math.round((s.count / data.funnel[i - 1].count) * 100)}% of previous</span> : null}</span>
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
        <Breakdown title="Countries" rows={data.countries} note="Marketing site only; console events carry no location." />
        <Breakdown title="Devices" rows={data.devices.filter((r) => r.name !== "(none)")} note="The sites do not send browser or device details yet." />
      </div>
      <p className="text-xs text-muted-foreground">Observed <Time iso={data.observed_at} relative />. Aggregates only; people who opt out or send Do Not Track are not counted. Retention needs a recurring event the sites do not send yet.</p>
    </div>
  )
}

/** Weekly cohorts by first machine pairing: the share still active n weeks later. */
function Retention({ cohorts }: { cohorts: Analytics["retention"] }) {
  const any = cohorts.some((c) => c.active.length > 0)
  return (
    <Panel title="Retention">
      {!any ? (
        <p className="text-sm text-muted-foreground" data-retention-state="not-configured">
          Not available yet: retention needs the console to send a daily activity event (console_active_day), which it does not send today.
        </p>
      ) : (
        <div className="overflow-x-auto" data-retention-state="ready">
          <table className="w-full border-collapse text-sm">
            <thead>
              <tr className="border-b border-divider text-left text-xs text-muted-foreground">
                <th className="h-8 px-2 font-medium">First paired in week of</th>
                <th className="px-2 text-right font-medium">Accounts</th>
                {cohorts[0].active.map((_, week) => (
                  <th key={week} className="px-2 text-right font-medium">Week {week}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {cohorts.map((c) => (
                <tr key={c.week} className="border-b border-divider">
                  <td className="h-8 px-2">{c.week.slice(0, 10)}</td>
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

function Breakdown({ title, rows, note }: { title: string; rows: { name: string; count: number }[]; note?: string }) {
  return (
    <Panel title={title}>
      {rows.length === 0 ? (
        <p className="text-sm text-muted-foreground">No data in this range.{note ? ` ${note}` : ""}</p>
      ) : (
        <ul className="border-t border-divider text-sm">
          {rows.map((r) => (
            <li key={r.name} className="flex items-center justify-between gap-3 border-b border-divider py-1.5">
              <span className="min-w-0 truncate">{r.name}</span>
              <span className="tabular-nums">{r.count}</span>
            </li>
          ))}
        </ul>
      )}
    </Panel>
  )
}
