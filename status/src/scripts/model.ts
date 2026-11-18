// The status page's logic, free of the DOM so it can be tested: what the banner says, how a day reads, how incidents group.
import { uptimeBar, type DayKind } from "../../lib/uptime.ts"

export type State = "operational" | "degraded" | "major" | "delayed" | "maintenance"
export interface Day { day: string; up: number; degraded: number; down: number; delayed: number }
export interface Unit { id: string; name: string; state: State }
export interface Component { id: string; name: string; note: string; state: State; since: number; units: Unit[]; days: Day[]; uptime: number | null }
export interface Update { status: string; body: string; ts: number }
export interface Incident { id: string; title: string; status: string; components: string[]; created: number; updated: number; resolved: number | null; updates: Update[] }
export interface Maintenance { id: string; title: string; body: string; components: string[]; starts: number; ends: number }
export interface Payload { now: number; at: number | null; delayed: boolean; public: { components: Component[]; incidents: Incident[]; maintenance: Maintenance[] } | null }

export const STALE_MS = 180_000
export const PHASES = ["investigating", "identified", "monitoring", "resolved"] as const
export const PHASE_WORD: Record<string, string> = { investigating: "Investigating", identified: "Identified", monitoring: "Monitoring", resolved: "Resolved" }
export const STATE_WORD: Record<State | "unknown", string> = { operational: "Operational", degraded: "Degraded", major: "Major outage", delayed: "Status data delayed", maintenance: "Maintenance", unknown: "Unknown" }

export type Overall = "operational" | "incident" | "degraded" | "major" | "maintenance" | "delayed" | "unavailable"

/** Never green without data: no payload, no snapshot, no components, or a decision older than 3 minutes is not "operational". */
export function overall(p: Payload | null): Overall {
  const data = p?.public
  if (!p || !data || data.components.length === 0) return "unavailable"
  if (p.delayed || p.at === null || p.now - p.at > STALE_MS) return "delayed"
  const s = data.components.map((c) => c.state)
  for (const x of ["major", "degraded", "maintenance", "delayed"] as const) if (s.includes(x)) return x
  // The checks are green but a person has declared an incident: the page says so rather than "all operational".
  return data.incidents.some((i) => i.status !== "resolved") ? "incident" : "operational"
}

export const BANNER: Record<Overall, { title: string; detail?: string }> = {
  operational: { title: "All systems operational" },
  incident: { title: "We're aware of an issue", detail: "Details are below." },
  degraded: { title: "Some systems are degraded", detail: "We are looking into it." },
  major: { title: "We're having a major outage", detail: "We are working on it." },
  maintenance: { title: "Scheduled maintenance in progress" },
  delayed: { title: "Status data delayed", detail: "We have not received fresh checks for over 3 minutes, so we cannot confirm anything is working. Services may be fine." },
  unavailable: { title: "Status unavailable", detail: "This page could not load the current status. It will try again shortly." },
}

/** What a component shows: its own state, or "unknown" when the whole page's data is not fresh. */
export const shownState = (o: Overall, c: { state: State }): State | "unknown" => (o === "delayed" || o === "unavailable" ? "unknown" : c.state)

const DAY = 86_400_000
export const dayStart = (day: string) => Date.parse(`${day}T00:00:00Z`)

/** The last `n` UTC days ending today, oldest first, with the recorded minutes (zeros when nothing was recorded). */
export function lastDays(days: Day[], now: number, n = 90): Day[] {
  const by = new Map(days.map((d) => [d.day, d]))
  return Array.from({ length: n }, (_, i) => {
    const day = new Date(now - (n - 1 - i) * DAY).toISOString().slice(0, 10)
    return by.get(day) ?? { day, up: 0, degraded: 0, down: 0, delayed: 0 }
  })
}

export const kindOf = (d: Day): DayKind => uptimeBar(d)

/** Incidents touching a component on a UTC day (from creation to resolution, or to now while open). */
export function incidentsOn(day: string, component: string, incidents: Incident[], now: number): Incident[] {
  const from = dayStart(day), to = from + DAY
  return incidents.filter((i) => i.components.includes(component) && i.created < to && (i.resolved ?? now) >= from)
}

export function dayLines(d: Day): string[] {
  const kind = kindOf(d)
  if (kind === "none") return ["No data for this day"]
  const lines: string[] = []
  if (d.down) lines.push(`${d.down} min major outage`)
  if (d.degraded) lines.push(`${d.degraded} min degraded`)
  if (d.delayed) lines.push(`${d.delayed} min not measured`)
  return lines.length ? lines : ["No downtime recorded"]
}

export function duration(ms: number): string {
  const m = Math.max(1, Math.round(ms / 60_000))
  if (m < 60) return `${m} min`
  const h = Math.floor(m / 60)
  if (h < 48) return m % 60 ? `${h} h ${m % 60} min` : `${h} h`
  return `${Math.round(h / 24)} days`
}

/** Resolved incidents by calendar month (UTC), newest month first, newest incident first within a month. */
export function byMonth(incidents: Incident[]): { month: string; items: Incident[] }[] {
  const groups = new Map<string, Incident[]>()
  for (const i of [...incidents].sort((a, b) => b.created - a.created)) {
    const key = new Date(i.created).toISOString().slice(0, 7)
    groups.set(key, [...(groups.get(key) ?? []), i])
  }
  return [...groups].map(([month, items]) => ({ month, items }))
}

export const phaseIndex = (status: string) => Math.max(0, PHASES.indexOf(status as (typeof PHASES)[number]))
