/** Minutes of one day by state; delayed minutes are not measured, so they are neither up nor down. */
export interface UptimeDay { day: string; up: number; degraded: number; down: number; delayed: number }


/** Measured minutes only: delayed minutes are not in the denominator, degraded counts 30% as down (the page's footnote says so). */
export const DEGRADED_WEIGHT = 0.3

/** Under three hours of measured minutes is not enough to claim a percentage; after that it is the figure for the tracked period. */
export const MIN_MEASURED_MINUTES = 180

export const uptimePercent = (days: UptimeDay[]): number | null => {
  const total = days.reduce((n, d) => n + d.up + d.degraded + d.down, 0)
  if (total < MIN_MEASURED_MINUTES) return null
  const lost = days.reduce((n, d) => n + d.down + DEGRADED_WEIGHT * d.degraded, 0)
  return Math.round(((total - lost) / total) * 10000) / 100
}

export type DayKind = "up" | "degraded" | "down" | "unknown" | "none"

/** The colour of one day's bar. A day mostly unmeasured is "unknown", never green. */
export const uptimeBar = (d: UptimeDay): DayKind =>
  d.up + d.degraded + d.down + d.delayed === 0 ? "none"
  : d.down >= 10 ? "down"
  : d.down > 0 || d.degraded >= 10 ? "degraded"
  : d.delayed >= 10 && d.delayed >= d.up ? "unknown"
  : "up"
