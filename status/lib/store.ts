import catalogJson from "../config/components.json" with { type: "json" }
import { decideComponent, decideUnit, WINDOW_MS, type Evidence, type Sample, type Series, type State } from "./decide.ts"
import type { Catalog, Incident, Maintenance, Snapshot, UptimeDay } from "./types.ts"
import { uptimePercent } from "./uptime.ts"

export const catalog = catalogJson as Catalog
const UNITS = new Map(catalog.components.flatMap((c) => c.units.map((u) => [u.id, { c, u }] as const)))
export const unitKnown = (id: string) => UNITS.has(id)

const DAY = 86_400_000
export const dayOf = (ms: number) => new Date(ms).toISOString().slice(0, 10)
export const minute = (ms: number) => Math.floor(ms / 60_000) * 60_000
export const HISTORY_DAYS = 90

export type Reported = Record<string, [number, number, string?]> // unit -> [ok 0/1, ms, error]

/** One row per vantage point and minute. ts is the receipt time unrounded (freshness is judged on it); bucket only dedupes the minute. */
export async function saveReport(db: D1Database, vantage: string, now: number, probeTs: number, netOk: boolean, build: string, results: Reported) {
  await db
    .prepare("INSERT OR REPLACE INTO reports (vantage, bucket, ts, probe_ts, net_ok, build, results) VALUES (?, ?, ?, ?, ?, ?, ?)")
    .bind(vantage, minute(now), now, probeTs, netOk ? 1 : 0, build.slice(0, 40), netOk ? JSON.stringify(results) : "{}")
    .run()
}

/** What a probe write must clear: the signed time must be newer than the last accepted one (replay), and posts are at least RATE_MS apart. */
export const RATE_MS = 20_000
export async function admitProbe(db: D1Database, vantage: string, now: number, probeTs: number): Promise<"ok" | "replayed" | "rate"> {
  const last = await db.prepare("SELECT MAX(probe_ts) AS probe_ts, MAX(ts) AS ts FROM reports WHERE vantage = ?").bind(vantage).first<{ probe_ts: number | null; ts: number | null }>()
  if (last?.probe_ts != null && probeTs <= last.probe_ts) return "replayed"
  if (last?.ts != null && now - last.ts < RATE_MS) return "rate"
  return "ok"
}

interface Row { vantage: string; ts: number; results: string }

/**
 * Re-decide every unit and component from the last minutes of reports and write the changes. Idempotent per minute: the uptime
 * counters advance once for a given minute however often it runs (a cron retry), and the evaluation time is what the page's
 * freshness is judged on.
 */
export async function evaluate(db: D1Database, now: number): Promise<Record<string, Evidence[]>> {
  const rows = (await db.prepare("SELECT vantage, ts, results FROM reports WHERE net_ok = 1 AND ts >= ? ORDER BY ts").bind(now - WINDOW_MS).all<Row>()).results
  const prev = new Map((await db.prepare("SELECT unit, state FROM unit_state").all<{ unit: string; state: string }>()).results.map((r) => [r.unit, r.state as State]))
  const counted = Number((await db.prepare("SELECT v FROM kv WHERE k = 'uptime_minute'").first<{ v: string }>())?.v ?? 0)
  const thisMinute = minute(now)
  const parsed = rows.map((r) => ({ vantage: r.vantage, ts: r.ts, results: JSON.parse(r.results) as Reported }))
  const writes: D1PreparedStatement[] = []
  const evidence: Record<string, Evidence[]> = {}
  const day = dayOf(now)
  const setState = (key: string, state: State) =>
    db.prepare("INSERT INTO unit_state (unit, state, since) VALUES (?, ?, ?) ON CONFLICT(unit) DO UPDATE SET state = excluded.state, since = excluded.since").bind(key, state, now)
  for (const comp of catalog.components) {
    const states: State[] = []
    for (const unit of comp.units) {
      const series: Series[] = unit.vantages.map((vantage) => ({
        vantage,
        samples: parsed.flatMap((r): Sample[] => {
          const hit = r.vantage === vantage ? r.results[unit.id] : undefined
          return hit ? [{ ts: r.ts, ok: hit[0] === 1, ms: hit[1] }] : []
        }),
      }))
      const old = prev.get(unit.id) ?? "operational"
      const d = decideUnit(old, now, series, unit.slowMs)
      evidence[unit.id] = d.evidence
      states.push(d.state)
      if (!prev.has(unit.id) || old !== d.state) writes.push(setState(unit.id, d.state))
    }
    const state = decideComponent(states)
    if (prev.get(`comp:${comp.id}`) !== state) writes.push(setState(`comp:${comp.id}`, state))
    if (thisMinute > counted && !(await inMaintenance(db, comp.id, now))) {
      const col = { operational: "up", degraded: "degraded", major: "down", delayed: "delayed" }[state]
      writes.push(db.prepare(`INSERT INTO uptime_day (component, day, ${col}) VALUES (?, ?, 1) ON CONFLICT(component, day) DO UPDATE SET ${col} = ${col} + 1`).bind(comp.id, day))
    }
  }
  const put = (k: string, v: string) => db.prepare("INSERT INTO kv (k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v").bind(k, v)
  writes.push(db.prepare("DELETE FROM reports WHERE ts < ?").bind(now - 3_600_000))
  writes.push(put("evidence", JSON.stringify(evidence)), put("evaluated_at", String(now)), put("uptime_minute", String(Math.max(counted, thisMinute))))
  await db.batch(writes)
  return evidence
}

async function inMaintenance(db: D1Database, component: string, now: number) {
  const rows = (await db.prepare("SELECT components FROM maintenance WHERE starts <= ? AND ends > ?").bind(now, now).all<{ components: string }>()).results
  return rows.some((r) => (JSON.parse(r.components) as string[]).includes(component))
}

/**
 * The snapshot the page reads. `at` is the last time units were decided, never the time of this rebuild: an admin write
 * (incident, maintenance) refreshes the incident text but cannot make old decisions look fresh.
 */
export async function buildSnapshot(db: D1Database, now: number): Promise<Snapshot> {
  const since = now - HISTORY_DAYS * DAY
  const [states, days, incidents, updates, maint, evid, heart, subs, evaluated] = await Promise.all([
    db.prepare("SELECT unit, state, since FROM unit_state").all<{ unit: string; state: State; since: number }>(),
    db.prepare("SELECT component, day, up, degraded, down, delayed FROM uptime_day WHERE day >= ? ORDER BY day").bind(dayOf(since)).all<UptimeDay & { component: string }>(),
    db.prepare("SELECT * FROM incidents WHERE resolved IS NULL OR resolved >= ? ORDER BY created DESC LIMIT 200").bind(since).all<{ id: string; title: string; status: string; components: string; created: number; updated: number; resolved: number | null }>(),
    db.prepare("SELECT incident_id, status, body, ts FROM incident_updates WHERE ts >= ? ORDER BY ts DESC").bind(since).all<{ incident_id: string; status: string; body: string; ts: number }>(),
    db.prepare("SELECT * FROM maintenance WHERE ends >= ? ORDER BY starts").bind(now - 7 * DAY).all<{ id: string; title: string; body: string; components: string; starts: number; ends: number }>(),
    db.prepare("SELECT v FROM kv WHERE k = 'evidence'").first<{ v: string }>(),
    db.prepare("SELECT vantage, MAX(ts) AS last, build, net_ok FROM reports GROUP BY vantage").all<{ vantage: string; last: number; build: string; net_ok: number }>(),
    db.prepare("SELECT COUNT(*) AS n FROM subscribers WHERE confirmed = 1").first<{ n: number }>(),
    db.prepare("SELECT v FROM kv WHERE k = 'evaluated_at'").first<{ v: string }>(),
  ])
  const state = new Map(states.results.map((r) => [r.unit, r]))
  const maintenance: Maintenance[] = maint.results.map((m) => ({ ...m, components: JSON.parse(m.components) }))
  const active = (id: string) => maintenance.some((m) => m.starts <= now && m.ends > now && m.components.includes(id))
  const components = catalog.components.map((c) => {
    const mine = days.results.filter((d) => d.component === c.id).map(({ day, up, degraded, down, delayed }) => ({ day, up, degraded, down, delayed }))
    const comp = state.get(`comp:${c.id}`)
    return {
      id: c.id, name: c.name, note: c.note,
      state: active(c.id) ? ("maintenance" as const) : (comp?.state ?? "delayed"),
      since: comp?.since ?? now,
      units: c.units.map((u) => ({ id: u.id, name: u.name ?? u.id, state: state.get(u.id)?.state ?? ("delayed" as State) })),
      days: mine, uptime: uptimePercent(mine),
    }
  })
  const byIncident = new Map<string, Incident["updates"]>()
  for (const u of updates.results) byIncident.set(u.incident_id, [...(byIncident.get(u.incident_id) ?? []), { status: u.status, body: u.body, ts: u.ts }])
  const snapshot: Snapshot = {
    at: Number(evaluated?.v ?? 0),
    public: {
      components,
      incidents: incidents.results.map((i) => ({ ...i, components: JSON.parse(i.components), updates: byIncident.get(i.id) ?? [] })),
      maintenance,
    },
    admin: {
      evidence: evid ? JSON.parse(evid.v) : {},
      vantages: catalog.vantages.map((v) => {
        const h = heart.results.find((r) => r.vantage === v)
        return { vantage: v, last: h?.last ?? null, net_ok: h ? h.net_ok === 1 : null, build: h?.build ?? "" }
      }),
      subscribers: subs?.n ?? 0,
    },
  }
  await db.prepare("INSERT INTO snapshot (id, at, body) VALUES (1, ?, ?) ON CONFLICT(id) DO UPDATE SET at = excluded.at, body = excluded.body").bind(snapshot.at, JSON.stringify(snapshot)).run()
  return snapshot
}

export async function readSnapshot(db: D1Database): Promise<Snapshot | null> {
  const row = await db.prepare("SELECT body FROM snapshot WHERE id = 1").first<{ body: string }>()
  return row ? (JSON.parse(row.body) as Snapshot) : null
}
