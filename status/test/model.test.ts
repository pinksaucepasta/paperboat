import { test } from "node:test"
import assert from "node:assert/strict"
import { ago, byMonth, dayLines, duration, incidentsOn, kindOf, lastDays, overall, shownState, type Component, type Incident, type Payload } from "../src/scripts/model.ts"

const NOW = Date.UTC(2026, 10, 18, 12, 0, 0)
const comp = (state: Component["state"]): Component => ({ id: "api", name: "API", note: "", state, since: 0, units: [], days: [], uptime: null })
const payload = (components: Component[], over: Partial<Payload> = {}): Payload => ({ now: NOW, at: NOW - 30_000, delayed: false, public: { components, incidents: [], maintenance: [] }, ...over })

test("the banner is never green without data", () => {
  assert.equal(overall(null), "unavailable")
  assert.equal(overall({ now: NOW, at: null, delayed: true, public: null }), "unavailable")
  assert.equal(overall(payload([])), "unavailable") // an empty list must not read "all operational"
  assert.equal(overall(payload([comp("operational")], { delayed: true })), "delayed")
  assert.equal(overall(payload([comp("operational")], { at: NOW - 181_000 })), "delayed")
  assert.equal(overall(payload([comp("operational")], { at: null })), "delayed")
  assert.equal(overall(payload([comp("operational")])), "operational")
})

test("a declared open incident over green checks is not \"all operational\"", () => {
  const p = payload([comp("operational")])
  p.public!.incidents = [{ id: "i", title: "t", status: "monitoring", components: ["api"], created: 0, updated: 0, resolved: null, updates: [] }]
  assert.equal(overall(p), "incident")
  p.public!.incidents[0].status = "resolved"
  assert.equal(overall(p), "operational")
})

test("worst state wins and stale data shows components as unknown", () => {
  assert.equal(overall(payload([comp("operational"), comp("degraded"), comp("major")])), "major")
  assert.equal(overall(payload([comp("operational"), comp("maintenance")])), "maintenance")
  assert.equal(overall(payload([comp("operational"), comp("delayed")])), "delayed")
  assert.equal(shownState("delayed", comp("operational")), "unknown")
  assert.equal(shownState("operational", comp("degraded")), "degraded")
})

test("lastDays always yields 90 days ending today and fills gaps with empty days", () => {
  const days = lastDays([{ day: "2026-11-17", up: 1440, degraded: 0, down: 0, delayed: 0 }], NOW)
  assert.equal(days.length, 90)
  assert.equal(days[89].day, "2026-11-18")
  assert.equal(days[88].up, 1440)
  assert.equal(days[0].up + days[0].down, 0)
})

test("day tooltips say what happened and never imply a clean day without data", () => {
  assert.deepEqual(dayLines({ day: "d", up: 0, degraded: 0, down: 0, delayed: 0 }), ["No data for this day"])
  assert.deepEqual(dayLines({ day: "d", up: 1440, degraded: 0, down: 0, delayed: 0 }), ["No downtime recorded"])
  assert.deepEqual(dayLines({ day: "d", up: 1400, degraded: 12, down: 20, delayed: 8 }), ["20 min major outage", "12 min degraded", "8 min not measured"])
})

const inc = (id: string, created: number, resolved: number | null, components = ["api"]): Incident => ({ id, title: id, status: resolved ? "resolved" : "investigating", components, created, updated: created, resolved, updates: [] })

test("incidents on a day: spans midnight, open incidents run to now, other components excluded", () => {
  const a = inc("a", Date.UTC(2026, 10, 16, 23, 0), Date.UTC(2026, 10, 17, 1, 0))
  const open = inc("open", Date.UTC(2026, 10, 17, 10, 0), null)
  const other = inc("o", Date.UTC(2026, 10, 17, 10, 0), null, ["edges"])
  const ids = (d: string) => incidentsOn(d, "api", [a, open, other], NOW).map((i) => i.id)
  assert.deepEqual(ids("2026-11-16"), ["a"])
  assert.deepEqual(ids("2026-11-17"), ["a", "open"])
  assert.deepEqual(ids("2026-11-18"), ["open"])
  assert.deepEqual(ids("2026-11-15"), [])
})

test("incidents group by month, newest first", () => {
  const g = byMonth([inc("x", Date.UTC(2026, 9, 5), 1), inc("y", Date.UTC(2026, 10, 2), 1), inc("z", Date.UTC(2026, 10, 9), 1)])
  assert.deepEqual(g.map((m) => [m.month, m.items.map((i) => i.id)]), [["2026-11", ["z", "y"]], ["2026-10", ["x"]]])
})

test("durations read naturally", () => {
  assert.equal(duration(30_000), "1 min")
  assert.equal(duration(72 * 60_000), "1 h 12 min")
  assert.equal(duration(3 * 3600_000), "3 h")
  assert.equal(duration(72 * 3600_000), "3 days")
})

test("90 days are always drawn: days before tracking started are empty (grey), not hidden", () => {
  const days = lastDays([{ day: "2026-11-18", up: 300, degraded: 0, down: 0, delayed: 0 }], NOW)
  assert.equal(days.length, 90)
  assert.deepEqual([kindOf(days[0]), kindOf(days[88]), kindOf(days[89])], ["none", "none", "up"])
})

test("'Updated … ago' reads naturally as it ticks", () => {
  assert.equal(ago(1000), "just now")
  assert.equal(ago(20_000), "20 s ago")
  assert.equal(ago(125_000), "2 min ago")
  assert.equal(ago(3 * 3600_000), "3 h ago")
})
