/**
 * Bandwidth periods, shared by the browser query (`lib/api.ts`) and the server prefetch, so both
 * ask the same question and land in the same cache entry.
 */
/** Periods the bandwidth card offers (the server buckets hourly up to 48 hours, daily after). */
export const usagePeriods = {
  "24h": { label: "Last 24 hours", hours: 24 },
  "7d": { label: "Last 7 days", hours: 24 * 7 },
  "30d": { label: "Last 30 days", hours: 24 * 30 },
} as const
export type UsagePeriod = keyof typeof usagePeriods
export const defaultUsagePeriod: UsagePeriod = "30d"

/** The query string for a period ending now; shared with the server prefetch. */
export function usageParams(workspace: string, period: UsagePeriod, now = new Date()): URLSearchParams {
  const start = new Date(now.getTime() - usagePeriods[period].hours * 3_600_000)
  return new URLSearchParams({ start: start.toISOString(), end: now.toISOString(), limit: "200", workspace })
}
