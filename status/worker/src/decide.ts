// The status state machine: pure functions, no I/O. Every rule here is a user rule (see README.md):
//  1. red only when >=2 independent vantage points fail 3 checks in a row
//  3. green returns after 5 passes in a row
//  4. degraded on latency or a partial failure
//  5. data older than 3 minutes is "delayed", never green
// Probe hosts that lose their own internet send no results (rule 2), so their series simply goes stale.

export type State = "operational" | "degraded" | "major" | "delayed"

export const FRESH_MS = 180_000 // a vantage point with no result newer than this does not count (rule 5)
export const WINDOW_MS = 600_000 // results older than this are not "in a row" any more
export const FAIL_RUN = 3
export const PASS_RUN = 5
export const CONFIRM = 2

export interface Sample {
  ts: number
  ok: boolean
  ms: number
}
export interface Series {
  vantage: string
  samples: Sample[] // oldest first
}
export type VantageView = "fail" | "slow" | "pass" | "mixed" | "stale"
export interface Evidence {
  vantage: string
  view: VantageView
  last?: Sample
}
export interface UnitDecision {
  state: State
  evidence: Evidence[]
}

const slow = (s: Sample, slowMs: number) => !s.ok || s.ms > slowMs
const tail = (series: Series, now: number, n: number): Sample[] | null => {
  const recent = series.samples.filter((s) => s.ts >= now - WINDOW_MS)
  return recent.length >= n ? recent.slice(-n) : null
}

export function view(series: Series, now: number, slowMs: number): Evidence {
  const last = series.samples[series.samples.length - 1]
  if (!last || last.ts < now - FRESH_MS) return { vantage: series.vantage, view: "stale", last }
  const three = tail(series, now, FAIL_RUN)
  const five = tail(series, now, PASS_RUN)
  let v: VantageView = "mixed"
  if (three && three.every((s) => !s.ok)) v = "fail"
  else if (three && three.every((s) => slow(s, slowMs))) v = "slow"
  else if (five && five.every((s) => !slow(s, slowMs))) v = "pass"
  return { vantage: series.vantage, view: v, last }
}

/** One unit (a check, or a check in one region) from every vantage point that reports it. */
export function decideUnit(prev: State, now: number, series: Series[], slowMs: number): UnitDecision {
  const evidence = series.map((s) => view(s, now, slowMs))
  const fresh = evidence.filter((e) => e.view !== "stale")
  if (fresh.length === 0) return { state: "delayed", evidence }
  const count = (v: VantageView) => fresh.filter((e) => e.view === v).length
  const failing = count("fail")
  const bad = failing + count("slow")
  if (failing >= CONFIRM) return { state: "major", evidence }
  if (bad >= CONFIRM) return { state: "degraded", evidence }
  // Not confirmed bad. A healthy unit stays healthy; a bad one needs the pass run before it is green again (rule 3).
  if (prev === "operational" || prev === "delayed") return { state: "operational", evidence }
  const recovered = count("pass") >= Math.min(CONFIRM, fresh.length)
  if (recovered) return { state: "operational", evidence }
  return { state: "degraded", evidence } // no longer confirmed red, not yet green
}

/** A component from its units (regions or checks): all down is major, some down or slow is degraded. */
export function decideComponent(units: State[]): State {
  if (units.length === 0) return "delayed"
  if (units.every((u) => u === "major")) return "major"
  if (units.some((u) => u === "major" || u === "degraded")) return "degraded"
  if (units.some((u) => u === "delayed")) return "delayed"
  return "operational"
}
