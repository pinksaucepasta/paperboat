"use client"

import { Panel, Stat } from "@/components/page"
import { Time } from "@/components/time"
import { ToggleGroup, ToggleGroupItem } from "@/components/ui/toggle-group"
import { cn } from "@/lib/utils"

/** ↑/↓ percent against the range before, "new" when there was nothing before, nothing when both are zero. */
export function Delta({ value, previous, points, worseUp }: { value: number; previous: number; points?: boolean; worseUp?: boolean }) {
  const good = (up: boolean) => (up !== Boolean(worseUp) ? "text-emerald-600 dark:text-emerald-400" : "text-destructive")
  if (value === 0 && previous === 0) return null
  if (points) {
    const diff = Math.round((value - previous) * 10) / 10
    if (diff === 0) return <span className="text-muted-foreground">no change</span>
    return <span className={good(diff > 0)}>{diff > 0 ? "↑" : "↓"} {Math.round(Math.abs(diff) * 10) / 10} pts</span>
  }
  if (previous === 0) return <span className="text-muted-foreground">new</span>
  const change = Math.round(((value - previous) / previous) * 100)
  if (change === 0) return <span className="text-muted-foreground">no change</span>
  return <span className={good(change > 0)}>{change > 0 ? "↑" : "↓"} {Math.abs(change)}%</span>
}

/** A headline number with its change against the previous range (and an optional extra line). */
export function Headline({ label, value, previous, note, suffix = "", points }: { label: string; value: number; previous: number; note?: React.ReactNode; suffix?: string; points?: boolean }) {
  return (
    <Stat label={label} value={`${value}${suffix}`} note={<span className="flex flex-wrap gap-x-2"><Delta value={value} previous={previous} points={points} />{note}</span>} />
  )
}

/** A short list (at most five rows), hidden when there is nothing to show. */
export function TopList({ title, rows }: { title: string; rows: { name: string; count: number; extra?: string }[] }) {
  if (rows.length === 0) return null
  return (
    <Panel title={title}>
      <ul className="text-sm">
        {rows.map((r) => (
          <li key={r.name} className="flex items-center justify-between gap-3 border-b border-divider py-2.5">
            <span className="min-w-0 truncate">{r.name}</span>
            <span className="tabular-nums">
              {r.count}
              {r.extra && <span className="ml-2 text-xs text-muted-foreground">{r.extra}</span>}
            </span>
          </li>
        ))}
      </ul>
    </Panel>
  )
}

/** Bars per day; hidden for fewer than two days (one bar says nothing). */
export function Trend({ title, days, format = String }: { title: string; days: { day: string; visitors: number }[]; format?: (n: number) => string }) {
  if (days.length < 2) return null
  const peak = Math.max(1, ...days.map((d) => d.visitors))
  return (
    <Panel title={title}>
      <ul role="img" className="flex h-32 items-end gap-0.5 border-b border-divider" aria-label={`${title}: ${days.map((d) => `${d.day} ${format(d.visitors)}`).join(", ")}`}>
        {days.map((d) => (
          <li key={d.day} title={`${d.day}: ${format(d.visitors)}`} className="flex-1 rounded-t-sm bg-primary/70" style={{ height: `${Math.max(2, (d.visitors / peak) * 100)}%` }} />
        ))}
      </ul>
    </Panel>
  )
}

export const sourceName = (name: string) => (name === "$direct" ? "Direct" : name)

const regions = typeof Intl.DisplayNames === "function" ? new Intl.DisplayNames(["en"], { type: "region" }) : undefined
export const countryName = (code: string) => {
  try {
    return regions?.of(code.toUpperCase()) ?? code
  } catch {
    return code
  }
}

/** The server's section error codes (posthogquery, revenue), in words; the code stays visible below for support. */
const insightFailure = (provider: string): Record<string, string> => ({
  unauthorized: `${provider} refused the API key.`,
  rate_limited: `${provider} is rate limiting; try again in a minute.`,
  provider_unavailable: `${provider} is down; try again later.`,
  rejected: `${provider} rejected the query.`,
  timeout: `${provider} took too long; try again.`,
  unexpected_response: `${provider} sent an answer the server couldn’t read.`,
})

/** The sections of a page whose provider failed while the rest loaded, each in words with its code. */
export function Failures({ errors, provider = "PostHog" }: { errors?: Record<string, string>; provider?: string }) {
  return (
    <>
      {Object.entries(errors ?? {}).map(([k, code]) => (
        <p key={k} role="alert" className="flex flex-col text-sm text-destructive" data-insight-error={k}>
          Couldn’t load {k.replaceAll("_", " ")}. {insightFailure(provider)[code] ?? "Try again later."}
          <span className="text-xs text-muted-foreground">
            Code: <code className="font-mono">{code}</code>
          </span>
        </p>
      ))}
    </>
  )
}

export function Observed({ at, online, url }: { at: string; online: number; url?: string }) {
  return (
    <p className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted-foreground">
      <span data-online>
        <span className={cn("mr-1.5 inline-block size-1.5 rounded-full", online > 0 ? "bg-emerald-500" : "bg-muted-foreground/40")} />
        {online} online now
      </span>
      <span>Updated <Time iso={at} relative /></span>
      {url && <a href={url} target="_blank" rel="noreferrer" className="underline-offset-4 hover:underline">Open in PostHog</a>}
    </p>
  )
}

/** A two- or three-way choice shown as joined buttons (a range, an app). */
export function Toggle<T extends string>({ values, value, onChange }: { values: [T, string][]; value: T; onChange: (next: T) => void }) {
  return (
    <ToggleGroup value={[value]} onValueChange={(next) => next[0] && onChange(next[0] as T)} variant="outline" spacing={0}>
      {values.map(([v, label]) => (
        <ToggleGroupItem key={v} value={v}>{label}</ToggleGroupItem>
      ))}
    </ToggleGroup>
  )
}
