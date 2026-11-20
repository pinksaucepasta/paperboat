"use client"

import Link from "next/link"
import { Headline } from "@/components/analytics"
import { ErrorBlock } from "@/components/error-block"
import { Mono, StatusBadge } from "@/components/filters"
import { Page, Panel, Stat } from "@/components/page"
import { Time } from "@/components/time"
import { Skeleton } from "@/components/ui/skeleton"
import { ApiError, useApiQuery, type Data } from "@/lib/api"
import { notConfiguredLine } from "@/lib/integrations"

type Overview = Data<"/v1/admin/overview">
type Site = Data<"/v1/admin/analytics/site">
type Console = Data<"/v1/admin/analytics/console">
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
    <div className="grid grid-cols-2 gap-3 md:grid-cols-3 lg:grid-cols-6" data-overview-ready={data ? "" : undefined}>
      <Stat label="Accounts" value={v((d) => sum(d.accounts))} note={n((d) => states(d.accounts))} />
      <Stat label="Machines" value={v((d) => d.hosts)} note={n((d) => `${d.hosts_ready} ready · ${d.hosts_stale} stale`)} />
      <Stat label="Relays" value={v((d) => d.relays)} note={n((d) => `${d.relays_ready} ready · ${d.relays_stale} stale`)} />
      <Stat label="Billing workspaces" value={v((d) => sum(d.billing_workspaces))} note={n((d) => states(d.billing_workspaces))} />
      <Stat label="Control operations" value={v((d) => sum(d.control_operations))} note={n((d) => states(d.control_operations))} />
      <Stat label="Failed mail" value={v((d) => sum(d.transactional_mail.filter((r) => r.state === "failed")))} note={n((d) => states(d.transactional_mail))} />
    </div>
  )
}

/** The last week in three numbers; the Analytics page has the rest. */
function Insights() {
  const site = useApiQuery<Site>(["analytics", "site", "7"], "/v1/admin/analytics/site?days=7", 30_000)
  const product = useApiQuery<Console>(["analytics", "console", "7"], "/v1/admin/analytics/console?days=7", 30_000)
  const notConfigured = [site.error, product.error].some((e) => e instanceof ApiError && e.code === "analytics_not_configured")
  return (
    <Panel title="Last 7 days" action={<Link href="/analytics" className="text-sm text-muted-foreground underline-offset-4 hover:underline">Analytics</Link>}>
      {notConfigured ? (
        <p className="py-6 text-center text-sm text-muted-foreground" data-analytics-state="not-configured">
          {notConfiguredLine.analytics_not_configured}
        </p>
      ) : site.isError || product.isError ? (
        <ErrorBlock what="analytics" error={(site.error ?? product.error)!} retry={() => { site.refetch(); product.refetch() }} />
      ) : !site.data || !product.data ? (
        <div className="grid grid-cols-3 gap-3">
          {["Visitors", "Signups", "Weekly active"].map((label) => (
            <Stat key={label} label={label} value={<Skeleton className="my-1.5 h-5 w-12" />} />
          ))}
        </div>
      ) : (
        <div className="grid grid-cols-3 gap-3" data-analytics-state="ready">
          <Headline label="Visitors" value={site.data.visitors.value} previous={site.data.visitors.previous} />
          <Headline label="Signups" value={site.data.signups.value} previous={site.data.signups.previous} />
          <Headline label="Weekly active" value={product.data.weekly_active.value} previous={product.data.weekly_active.previous} />
        </div>
      )}
    </Panel>
  )
}
