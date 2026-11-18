import type { Evidence, State } from "./decide.ts"
import type { UptimeDay } from "./uptime.ts"
export type { UptimeDay }

export interface Env {
  DB: D1Database
  /** static assets of the page (absent in the local staging/demo Workers) */
  ASSETS?: Fetcher
  /** secret: JSON {"<vantage>": "<hex key>"} for the probe hosts that may write results */
  PROBE_KEYS: string
  /** secret: bearer token of the admin panel */
  ADMIN_TOKEN: string
  /** secret: JSON [{unit, url, expect?}] the Worker's own vantage point fetches every minute */
  CHECKS?: string
  /** var: a URL that must answer for this Worker's own internet to count (rule 2) */
  NET_CHECK_URL?: string
  /** vars/secrets for incident mail (the same Resend-style API the control plane uses); mail is off without them */
  MAIL_ENDPOINT?: string
  MAIL_KEY?: string
  MAIL_FROM?: string
  /** var: the page's origin (the API is same-origin): confirm, unsubscribe and feed links */
  PUBLIC_ORIGIN?: string
}

export interface UnitSpec { id: string; name?: string; vantages: string[]; slowMs: number }
export interface ComponentSpec { id: string; name: string; note: string; units: UnitSpec[] }
export interface Catalog { vantages: string[]; components: ComponentSpec[] }

export type Shown = State | "maintenance"
export interface Incident {
  id: string; title: string; status: string; components: string[]
  created: number; updated: number; resolved: number | null
  updates: { status: string; body: string; ts: number }[]
}
export interface Maintenance { id: string; title: string; body: string; components: string[]; starts: number; ends: number }
export interface PublicComponent {
  id: string; name: string; note: string; state: Shown; since: number
  units: { id: string; name: string; state: State }[]
  days: UptimeDay[]; uptime: number | null
}
export interface Snapshot {
  at: number
  public: { components: PublicComponent[]; incidents: Incident[]; maintenance: Maintenance[] }
  admin: {
    evidence: Record<string, Evidence[]>
    vantages: { vantage: string; last: number | null; net_ok: boolean | null; build: string }[]
    subscribers: number
  }
}
export const INCIDENT_STATUSES = ["investigating", "identified", "monitoring", "resolved"] as const
