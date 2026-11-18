"use client"

import { useState } from "react"
import { Failures, Headline, Observed, Toggle, TopList, Trend, countryName, sourceName } from "@/components/analytics"
import { ErrorBlock } from "@/components/error-block"
import { Page, Panel } from "@/components/page"
import { Skeleton } from "@/components/ui/skeleton"
import { dropoff } from "@/lib/analytics"
import { ApiError, useApiQuery, type Data } from "@/lib/api"
import { notConfiguredLine } from "@/lib/integrations"

type Site = Data<"/v1/admin/analytics/site">
type Console = Data<"/v1/admin/analytics/console">
type Days = "7" | "30" | "90"

const features: Record<string, string> = { terminal: "Terminals", exec: "Remote commands", ssh: "SSH", transfer: "File transfers", preview: "Previews", tunnel: "Tunnels", env: "ENV", recipe: "Recipes" }
const percent = (part: number, whole: number) => (whole > 0 ? Math.round((part / whole) * 100) : 0)

export default function AnalyticsPage() {
  const [app, setApp] = useState<"site" | "console">("site")
  const [days, setDays] = useState<Days>("30")
  return (
    <Page title="Analytics">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <Toggle values={[["site", "Website"], ["console", "Console"]]} value={app} onChange={setApp} />
        <Toggle values={[["7", "7 days"], ["30", "30 days"], ["90", "90 days"]]} value={days} onChange={setDays} />
      </div>
      {app === "site" ? <SiteView days={days} /> : <ConsoleView days={days} />}
    </Page>
  )
}

function Unavailable({ error, retry }: { error: Error; retry: () => void }) {
  if (error instanceof ApiError && error.code === "analytics_not_configured") {
    return <p className="py-12 text-center text-sm text-muted-foreground" data-analytics-state="not-configured">{notConfiguredLine.analytics_not_configured}</p>
  }
  return <ErrorBlock what="analytics" error={error} retry={retry} />
}

function Loading() {
  return <Skeleton className="h-64 w-full" />
}

function SiteView({ days }: { days: Days }) {
  const q = useApiQuery<Site>(["analytics", "site", days], `/v1/admin/analytics/site?days=${days}`, 30_000)
  if (q.isError) return <Unavailable error={q.error} retry={() => q.refetch()} />
  if (!q.data) return <Loading />
  const d = q.data
  const rate = (part: number, whole: number) => (whole > 0 ? `${((part / whole) * 100).toFixed(1)}% of visitors` : undefined)
  return (
    <div className="flex flex-col gap-6" data-analytics-state="ready" data-analytics-app="site">
      <Failures errors={d.errors} />
      <div className="grid grid-cols-2 gap-3">
        <Headline label="Visitors" value={d.visitors.value} previous={d.visitors.previous} />
        <Headline label="Signups" value={d.signups.value} previous={d.signups.previous} note={rate(d.signups.value, d.visitors.value)} />
      </div>
      <Trend title="Visitors per day" days={d.daily} />
      <div className="grid gap-6 md:grid-cols-2">
        <TopList title="Referrers" rows={d.referrers.map((r) => ({ name: sourceName(r.name), count: r.visitors, extra: `${r.signup_clicks} clicked sign up` }))} />
        <TopList title="Campaigns" rows={d.campaigns.map((r) => ({ name: r.name, count: r.visitors, extra: `${r.signup_clicks} clicked sign up` }))} />
        <TopList title="Pages" rows={d.pages} />
        <TopList title="Countries" rows={d.countries.map((r) => ({ name: countryName(r.name), count: r.count }))} />
      </div>
      <Observed at={d.observed_at} online={d.online} url={d.url} />
    </div>
  )
}

function ConsoleView({ days }: { days: Days }) {
  const q = useApiQuery<Console>(["analytics", "console", days], `/v1/admin/analytics/console?days=${days}`, 30_000)
  if (q.isError) return <Unavailable error={q.error} retry={() => q.refetch()} />
  if (!q.data) return <Loading />
  const d = q.data
  const r = d.retention
  const drop = dropoff(d.funnel)
  const adoption = d.adoption.filter((a) => features[a.name]).sort((a, b) => b.count - a.count)
  return (
    <div className="flex flex-col gap-6" data-analytics-state="ready" data-analytics-app="console">
      <Failures errors={d.errors} />
      <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
        <Headline label="Daily active" value={d.daily_active.value} previous={d.daily_active.previous} />
        <Headline label="Weekly active" value={d.weekly_active.value} previous={d.weekly_active.previous} />
        <Headline label="Paid conversions" value={d.paid.value} previous={d.paid.previous} />
        {r.size > 0 && <Headline label="Week-1 retention" suffix="%" points value={percent(r.retained, r.size)} previous={percent(r.previous_retained, r.previous_size)} note={`${r.retained} of ${r.size} accounts`} />}
      </div>
      {d.funnel.length > 0 && (
        <Panel title="Activation">
          <ol className="text-sm" data-funnel>
            {d.funnel.map((s, i) => (
              <li key={s.step} data-dropoff={i === drop ? "" : undefined} className="flex items-center justify-between border-b border-divider py-2.5">
                <span>{s.step}{i === drop && <span className="ml-2 text-xs text-destructive">biggest drop</span>}</span>
                <span className="tabular-nums">{s.count}<span className="ml-2 inline-block w-10 text-right text-xs text-muted-foreground">{i > 0 ? `${percent(s.count, d.funnel[i - 1].count)}%` : ""}</span></span>
              </li>
            ))}
          </ol>
        </Panel>
      )}
      {adoption.length > 0 && d.active_accounts > 0 && (
        <Panel title="Feature use">
          <ul className="text-sm" data-adoption>
            {adoption.map((a) => (
              <li key={a.name} className="flex items-center justify-between gap-3 border-b border-divider py-2.5">
                <span>{features[a.name]}</span>
                <span className="tabular-nums">{percent(a.count, d.active_accounts)}%<span className="ml-2 text-xs text-muted-foreground">{a.count} of {d.active_accounts} active</span></span>
              </li>
            ))}
          </ul>
        </Panel>
      )}
      <Observed at={d.observed_at} online={d.online} url={d.url} />
    </div>
  )
}
