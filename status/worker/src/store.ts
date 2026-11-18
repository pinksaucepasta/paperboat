import catalogJson from "../../config/components.json" with { type: "json" }
import { decideComponent, decideUnit, WINDOW_MS, type Evidence, type Sample, type Series, type State } from "./decide.ts"
import type { Catalog, Incident, Maintenance, Snapshot, UptimeDay } from "./types.ts"

export const catalog = catalogJson as Catalog
const UNITS = new Map(catalog.components.flatMap((c) => c.units.map((u) => [u.id, { c, u }] as const)))
export const unitKnown = (id: string) => UNITS.has(id)

const DAY = 86_400_000
export const dayOf = (ms: number) => new Date(ms).toISOString().slice(0, 10)
export const minute = (ms: number) => Math.floor(ms / 60_000) * 60_000

export type Reported = Record<string, [number, number, string?]> // unit -> [ok 0/1, ms, error]

export async function saveReport(db: D1Database, vantage: string, now: number, netOk: boolean, build: string, results: Reported) {
  await db
    .prepare("INSERT OR REPLACE INTO reports (vantage, ts, net_ok, build, results) VALUES (?, ?, ?, ?, ?)")
    .bind(vantage, minute(now), netOk ? 1 : 0, build.slice(0, 40), netOk ? JSON.stringify(results) : "{}")
    .run()
}

interface Row { vantage: string; ts: number; net_ok: number; results: string }

/** Re-decide every unit and component from the last minutes of reports, write the changes, count the minute for the uptime bars. */
export async function evaluate(db: D1Database, now: number): Promise<Record<string, Evidence[]>> {
  const rows = (await db.prepare("SELECT vantage, ts, net_ok, results FROM reports WHERE ts >= ? ORDER BY ts").bind(now - WINDOW_MS).all<Row>()).results
  const prev = new Map((await db.prepare("SELECT unit, state FROM unit_state").all<{ unit: string; state: string }>()).results.map((r) => [r.unit, r.state as State]))
  const parsed = rows.filter((r) => r.net_ok === 1).map((r) => ({ vantage: r.vantage, ts: r.ts, results: JSON.parse(r.results) as Reported }))
  const writes: D1PreparedStatement[] = []
  const evidence: Record<string, Evidence[]> = {}
  const day = dayOf(now)
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
      if (!prev.has(unit.id) || old !== d.state)
        writes.push(db.prepare("INSERT INTO unit_state (unit, state, since) VALUES (?, ?, ?) ON CONFLICT(unit) DO UPDATE SET state = excluded.state, since = excluded.since").bind(unit.id, d.state, now))
    }
    const state = decideComponent(states)
    const key = `comp:${comp.id}`
    if (prev.get(key) !== state)
      writes.push(db.prepare("INSERT INTO unit_state (unit, state, since) VALUES (?, ?, ?) ON CONFLICT(unit) DO UPDATE SET state = excluded.state, since = excluded.since").bind(key, state, now))
    if (state !== "delayed" && !(await inMaintenance(db, comp.id, now))) {
      const col = state === "operational" ? "up" : state === "degraded" ? "degraded" : "down"
      writes.push(db.prepare(`INSERT INTO uptime_day (component, day, ${col}) VALUES (?, ?, 1) ON CONFLICT(component, day) DO UPDATE SET ${col} = ${col} + 1`).bind(comp.id, day))
    }
  }
  writes.push(db.prepare("DELETE FROM reports WHERE ts < ?").bind(now - 3_600_000))
  writes.push(db.prepare("INSERT INTO kv (k, v) VALUES ('evidence', ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v").bind(JSON.stringify(evidence)))
  await db.batch(writes)
  return evidence
}

async function inMaintenance(db: D1Database, component: string, now: number) {
  const rows = (await db.prepare("SELECT components FROM maintenance WHERE starts <= ? AND ends > ?").bind(now, now).all<{ components: string }>()).results
  return rows.some((r) => (JSON.parse(r.components) as string[]).includes(component))
}

export const uptimeBar = (d: UptimeDay): "up" | "degraded" | "down" | "none" =>
  d.up + d.degraded + d.down === 0 ? "none" : d.down >= 10 ? "down" : d.down > 0 || d.degraded >= 10 ? "degraded" : "up"

export async function buildSnapshot(db: D1Database, now: number): Promise<Snapshot> {
  const [states, days, incidents, updates, maint, evid, heart, subs] = await Promise.all([
    db.prepare("SELECT unit, state, since FROM unit_state").all<{ unit: string; state: State; since: number }>(),
    db.prepare("SELECT component, day, up, degraded, down FROM uptime_day WHERE day >= ? ORDER BY day").bind(dayOf(now - 89 * DAY)).all<UptimeDay & { component: string }>(),
    db.prepare("SELECT * FROM incidents WHERE resolved IS NULL OR resolved >= ? ORDER BY created DESC").bind(now - 14 * DAY).all<{ id: string; title: string; status: string; components: string; created: number; updated: number; resolved: number | null }>(),
    db.prepare("SELECT incident_id, status, body, ts FROM incident_updates WHERE ts >= ? ORDER BY ts DESC").bind(now - 60 * DAY).all<{ incident_id: string; status: string; body: string; ts: number }>(),
    db.prepare("SELECT * FROM maintenance WHERE ends >= ? ORDER BY starts").bind(now).all<{ id: string; title: string; body: string; components: string; starts: number; ends: number }>(),
    db.prepare("SELECT v FROM kv WHERE k = 'evidence'").first<{ v: string }>(),
    db.prepare("SELECT vantage, MAX(ts) AS last, build, net_ok FROM reports GROUP BY vantage").all<{ vantage: string; last: number; build: string; net_ok: number }>(),
    db.prepare("SELECT COUNT(*) AS n FROM subscribers WHERE confirmed = 1").first<{ n: number }>(),
  ])
  const state = new Map(states.results.map((r) => [r.unit, r]))
  const maintenance: Maintenance[] = maint.results.map((m) => ({ ...m, components: JSON.parse(m.components) }))
  const active = (id: string) => maintenance.some((m) => m.starts <= now && m.ends > now && m.components.includes(id))
  const components = catalog.components.map((c) => {
    const mine = days.results.filter((d) => d.component === c.id)
    const total = mine.reduce((n, d) => n + d.up + d.degraded + d.down, 0)
    const down = mine.reduce((n, d) => n + d.down, 0)
    const comp = state.get(`comp:${c.id}`)
    return {
      id: c.id, name: c.name, note: c.note,
      state: active(c.id) ? ("maintenance" as const) : (comp?.state ?? "delayed"),
      since: comp?.since ?? now,
      units: c.units.map((u) => ({ id: u.id, name: u.name ?? u.id, state: state.get(u.id)?.state ?? ("delayed" as State) })),
      days: mine.map(({ day, up, degraded, down }) => ({ day, up, degraded, down })),
      uptime: total ? Math.round(((total - down) / total) * 10000) / 100 : null,
    }
  })
  const byIncident = new Map<string, Incident["updates"]>()
  for (const u of updates.results) byIncident.set(u.incident_id, [...(byIncident.get(u.incident_id) ?? []), { status: u.status, body: u.body, ts: u.ts }])
  const snapshot: Snapshot = {
    at: now,
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
  await db.prepare("INSERT INTO snapshot (id, at, body) VALUES (1, ?, ?) ON CONFLICT(id) DO UPDATE SET at = excluded.at, body = excluded.body").bind(now, JSON.stringify(snapshot)).run()
  return snapshot
}

export async function readSnapshot(db: D1Database): Promise<Snapshot | null> {
  const row = await db.prepare("SELECT body FROM snapshot WHERE id = 1").first<{ body: string }>()
  return row ? (JSON.parse(row.body) as Snapshot) : null
}
