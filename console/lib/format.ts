const units = ["B", "KB", "MB", "GB", "TB", "PB"]

export function formatBytes(bytes: number): string {
  let value = bytes
  let unit = 0
  while (value >= 1000 && unit < units.length - 1) {
    value /= 1000
    unit++
  }
  const digits = unit === 0 || value >= 100 ? 0 : 1
  return `${value.toFixed(digits)} ${units[unit]}`
}

const relative = new Intl.RelativeTimeFormat("en", { numeric: "auto" })
const steps: [Intl.RelativeTimeFormatUnit, number][] = [
  ["year", 31_536_000],
  ["month", 2_592_000],
  ["week", 604_800],
  ["day", 86_400],
  ["hour", 3_600],
  ["minute", 60],
]

/** "3 minutes ago" from `now`. In components use `useTimeFormat` (it supplies a hydration-safe now). */
export function formatRelative(iso: string, now: number): string {
  // useNow() is 0 until hydration: no clock yet, so the stable form.
  if (!now) return formatStable(iso)
  const seconds = Math.round((new Date(iso).getTime() - now) / 1000)
  for (const [unit, size] of steps) {
    if (Math.abs(seconds) >= size) return relative.format(Math.round(seconds / size), unit)
  }
  return "just now"
}

/*
 * Timestamps arrive from the server in UTC. They are shown in the viewer's own time zone with
 * the zone named (as Tailscale and NetBird do), and the exact UTC instant is one hover away.
 */
const dateTime = new Intl.DateTimeFormat(undefined, {
  dateStyle: "medium",
  timeStyle: "short",
})
const zone = new Intl.DateTimeFormat(undefined, { timeZoneName: "short" })
const exactLocal = new Intl.DateTimeFormat(undefined, {
  dateStyle: "medium",
  timeStyle: "long",
})
const exactUtc = new Intl.DateTimeFormat(undefined, {
  dateStyle: "medium",
  timeStyle: "long",
  timeZone: "UTC",
})

/** Dates in the viewer's locale and zone, by purpose. */
const dateStyles = {
  /** Oct 22 */
  day: { month: "short", day: "numeric" },
  /** Oct 22, 2026 */
  date: { year: "numeric", month: "short", day: "numeric" },
  /** Thursday, October 22 */
  weekday: { weekday: "long", month: "long", day: "numeric" },
  /** 7:34 PM */
  time: { hour: "numeric", minute: "2-digit" },
  /** 19:34:05, for request logs */
  seconds: { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false },
  /** 7 PM */
  hour: { hour: "numeric" },
  /** October 2026 */
  month: { month: "long", year: "numeric" },
} satisfies Record<string, Intl.DateTimeFormatOptions>
export type DateStyle = keyof typeof dateStyles
const dateFormatters = Object.fromEntries(
  Object.entries(dateStyles).map(([style, options]) => [style, new Intl.DateTimeFormat(undefined, options)]),
) as Record<DateStyle, Intl.DateTimeFormat>

/** A date in the viewer's locale and zone. In components use `useTimeFormat().date`. */
export function formatDate(iso: string, style: DateStyle): string {
  return dateFormatters[style].format(new Date(iso))
}

/** The same date in a form that needs no locale or zone (UTC), for the first render. */
export function formatDateStable(iso: string, style: DateStyle): string {
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) return ""
  const text = date.toISOString()
  if (style === "time") return `${text.slice(11, 16)} UTC`
  if (style === "seconds") return `${text.slice(11, 19)} UTC`
  if (style === "hour") return `${text.slice(11, 13)}:00 UTC`
  if (style === "month") return text.slice(0, 7)
  return text.slice(0, 10)
}

/**
 * "2026-08-31 23:56 UTC": the same on the server and every browser, for the first render before
 * the viewer's clock, zone and locale may be used (they differ between server and browser).
 */
export function formatStable(iso: string): string {
  const date = new Date(iso)
  return Number.isNaN(date.getTime()) ? "" : `${date.toISOString().slice(0, 16).replace("T", " ")} UTC`
}

/** "Aug 31, 2026, 5:26 AM GMT+5:30" in the viewer's zone. */
export function formatDateTime(iso: string): string {
  const date = new Date(iso)
  const name = zone.formatToParts(date).find((part) => part.type === "timeZoneName")?.value
  return name ? `${dateTime.format(date)} ${name}` : dateTime.format(date)
}

/** Both exact forms, for a tooltip: local with zone, then UTC. */
export function formatExact(iso: string): { local: string; utc: string } {
  const date = new Date(iso)
  return { local: exactLocal.format(date), utc: exactUtc.format(date) }
}

const platforms: Record<string, string> = {
  darwin: "macOS",
  macos: "macOS",
  linux: "Linux",
  windows: "Windows",
  freebsd: "FreeBSD",
}

export function formatPlatform(platform: string): string {
  return platforms[platform.toLowerCase()] ?? platform
}

/** A region code as a place name: `in-mumbai` → Mumbai, `us-east` → Us East. */
export function placeName(region: string): string {
  const parts = region.toLowerCase().split(/[-_]/).filter(Boolean)
  // A country prefix goes, unless what remains is only a direction ("us-east" stays "Us East").
  const compass = /^(north|south|east|west|central)$/
  if (parts.length > 1 && /^[a-z]{2}$/.test(parts[0]) && !(parts.length === 2 && compass.test(parts[1]))) parts.shift()
  if (parts.length > 1 && /^\d+$/.test(parts[parts.length - 1])) parts.pop()
  return parts.map((part) => part.charAt(0).toUpperCase() + part.slice(1)).join(" ") || region
}
