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

export function formatRelative(iso: string, now = Date.now()): string {
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

/** Paperboat's own machine domain; mirrors the server's `machineservices.BrowserSuffix`. */
export const machineDomain = process.env.NEXT_PUBLIC_MACHINE_DOMAIN

/** A region code as a place name: `in-mumbai` → Mumbai, `us-east` → Us East. */
export function placeName(region: string): string {
  const parts = region.toLowerCase().split(/[-_]/).filter(Boolean)
  // A country prefix goes, unless what remains is only a direction ("us-east" stays "Us East").
  const compass = /^(north|south|east|west|central)$/
  if (parts.length > 1 && /^[a-z]{2}$/.test(parts[0]) && !(parts.length === 2 && compass.test(parts[1]))) parts.shift()
  if (parts.length > 1 && /^\d+$/.test(parts[parts.length - 1])) parts.pop()
  return parts.map((part) => part.charAt(0).toUpperCase() + part.slice(1)).join(" ") || region
}
