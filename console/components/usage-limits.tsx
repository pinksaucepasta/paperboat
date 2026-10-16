"use client"

import {
  AppleLogoIcon,
  ArrowDownIcon,
  ArrowUpIcon,
  CaretDownIcon,
  LinuxLogoIcon,
  WindowsLogoIcon,
  type Icon,
} from "@phosphor-icons/react"
import { useId, useState } from "react"
import {
  billingQuery,
  trafficBreakdown,
  useApiQuery,
  usageQuery,
  useMachines,
  useWorkspace,
  type BillingWorkspace,
  type Machine,
  type PathMode,
  type TrafficSource,
  type Usage,
} from "@/lib/api"
import { formatBytes, formatPlatform } from "@/lib/format"
import { filterKeys, parseQuery, platformToken, withFilter, type FilterKey } from "@/lib/machine-filter"
import { cn } from "@/lib/utils"
import { defaultUsagePeriod, usagePeriods, type UsagePeriod } from "@/lib/usage-period"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { useMachineQuery } from "@/components/machine-query"
import { Skeleton } from "@/components/ui/skeleton"

const sources: Record<TrafficSource, string> = {
  all: "Combined",
  managed: "Managed",
  self_hosted: "Self-hosted",
}

export function UsageLimits() {
  const workspace = useWorkspace()
  const machines = useMachines()
  const billing = useApiQuery(billingQuery(workspace))
  const [period, setPeriod] = useState<UsagePeriod>(defaultUsagePeriod)
  const [source, setSource] = useState<TrafficSource>("all")
  const usage = useApiQuery({ ...usageQuery(workspace?.id ?? "", period), enabled: Boolean(workspace) })

  return (
    // Machines and what is left of the hosted allowance first, then bandwidth across the other half.
    <section className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
      <Card label="Machines" loading={!workspace || machines.isPending} failed={machines.isError}>
        {machines.data && <Machines machines={machines.data} />}
      </Card>
      <Card label="Hosted allowance" loading={!workspace || billing.isPending} failed={billing.isError}>
        {billing.data && <Allowance billing={billing.data} />}
      </Card>
      <Card
        label="Bandwidth"
        action={
          <span className="flex items-center gap-3">
            <Menu value={source} options={sources} onChange={setSource} />
            <Menu
              value={period}
              options={Object.fromEntries(Object.entries(usagePeriods).map(([key, { label }]) => [key, label]))}
              onChange={(next) => setPeriod(next as UsagePeriod)}
            />
          </span>
        }
        loading={!workspace || usage.isPending}
        failed={usage.isError}
        className="sm:col-span-2"
      >
        {usage.data && <Bandwidth usage={usage.data} source={source} />}
      </Card>
    </section>
  )
}

/** A label that is also a control: only the word reacts, no box around it. */
const textControl =
  "rounded-sm outline-none underline-offset-4 decoration-foreground/30 hover:text-foreground hover:underline focus-visible:ring-2 focus-visible:ring-ring/30"

const platformIcons: Record<string, Icon> = { macos: AppleLogoIcon, linux: LinuxLogoIcon, windows: WindowsLogoIcon }

/**
 * Online and offline side by side (how many can be reached right now), then one quiet line of
 * what they run on.
 */
function Machines({ machines }: { machines: Machine[] }) {
  const [query, setQuery] = useMachineQuery()
  const { filters } = parseQuery(query)
  // Clicking a label filters the table below to it; clicking it again clears that filter.
  const toggle = (key: FilterKey, value: string) => {
    setQuery(withFilter(query, key, filters[key] === value ? undefined : value))
    document.getElementById("machines-table")?.scrollIntoView({ behavior: "smooth", block: "nearest" })
  }
  const online = machines.filter((machine) => machine.online).length
  const platforms = new Map<string, number>()
  for (const machine of machines) {
    const key = platformToken(machine.platform) ?? "unknown"
    platforms.set(key, (platforms.get(key) ?? 0) + 1)
  }
  return (
    <div className="flex flex-1 flex-col justify-between gap-4">
      <div className="grid grid-cols-2 gap-4">
        <Stat
          value={online}
          label="Online"
          dot="bg-success"
          active={filters.status === "online"}
          onClick={() => toggle("status", "online")}
        />
        <Stat
          value={machines.length - online}
          label="Offline"
          dot="bg-muted-foreground/40"
          muted={machines.length === online}
          active={filters.status === "offline"}
          onClick={() => toggle("status", "offline")}
        />
      </div>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-sm text-muted-foreground">
        {[...platforms]
          .sort((a, b) => b[1] - a[1])
          .map(([platform, count]) => {
            const PlatformIcon = platformIcons[platform]
            const name = formatPlatform(platform === "macos" ? "darwin" : platform)
            const filterable = platform in filterKeys.os.options
            return (
              <span key={platform} className="flex items-center gap-1.5">
                <button
                  type="button"
                  title={filterable ? `Show ${name} machines` : name}
                  aria-label={name}
                  disabled={!filterable}
                  aria-pressed={filters.os === platform}
                  onClick={() => toggle("os", platform)}
                  className={cn(
                    "flex rounded-sm outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/30 disabled:pointer-events-none",
                    filters.os === platform && "text-foreground",
                  )}
                >
                  {PlatformIcon ? (
                    <PlatformIcon weight="fill" className="size-4" />
                  ) : (
                    <span className="trim-cap">{name}</span>
                  )}
                </button>
                <span className="text-foreground trim-cap">{count}</span>
              </span>
            )
          })}
      </div>
    </div>
  )
}
function Stat({
  value,
  label,
  dot,
  muted,
  active,
  onClick,
}: {
  value: number
  label: string
  dot: string
  muted?: boolean
  active: boolean
  onClick: () => void
}) {
  return (
    <div className="flex flex-col gap-1">
      <Figure>
        <span className={muted ? "text-muted-foreground" : undefined}>{value}</span>
      </Figure>
      <span className="flex items-center gap-1.5 text-sm text-muted-foreground">
        <span className={cn("size-2 shrink-0 rounded-full", dot)} />
        <button
          type="button"
          aria-pressed={active}
          title={active ? "Show all machines" : `Show ${label.toLowerCase()} machines`}
          onClick={onClick}
          className={cn(textControl, "trim-cap", active && "text-foreground underline")}
        >
          {label}
        </button>
      </span>
    </div>
  )
}
const resetFormat = new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric" })

/**
 * What is left of this period's allowance for traffic through Paperboat's hosted relays and
 * edge. Direct and self-hosted traffic are free and not counted here.
 */
function Allowance({ billing }: { billing: BillingWorkspace }) {
  const used = billing.usage_bytes
  const allowance = billing.allowance_bytes
  const resets = billing.period_end ? `Resets ${resetFormat.format(new Date(billing.period_end))}` : null
  if (allowance === null) {
    return (
      <div className="flex flex-col gap-1.5">
        <Figure>{formatBytes(used)}</Figure>
        <span className="text-xs text-muted-foreground">Used, no limit{resets && ` · ${resets}`}</span>
      </div>
    )
  }
  const share = allowance > 0 ? Math.min(1, used / allowance) : 1
  const out = used >= allowance
  return (
    <div className="flex flex-1 flex-col gap-1.5">
      <span className="flex items-baseline gap-2">
        <Figure>{formatBytes(Math.max(0, allowance - used))}</Figure>
        <span className="text-sm text-muted-foreground">left</span>
      </span>
      <span className="text-xs text-muted-foreground">
        {formatBytes(used)} of {formatBytes(allowance)} used{resets && ` · ${resets}`}
      </span>
      <div
        role="meter"
        aria-label="Hosted allowance used"
        aria-valuemin={0}
        aria-valuemax={allowance}
        aria-valuenow={Math.min(used, allowance)}
        className="mt-auto h-1.5 overflow-hidden rounded-full bg-muted"
      >
        <div
          className={cn("h-full rounded-full", out ? "bg-destructive" : "bg-primary")}
          style={{ width: `${share * 100}%` }}
        />
      </div>
    </div>
  )
}
const paths: { mode: PathMode; label: string; color: string; dot: string }[] = [
  { mode: "direct", label: "Direct", color: "text-path-direct", dot: "bg-path-direct" },
  { mode: "relay", label: "Relay", color: "text-path-relay", dot: "bg-path-relay" },
  { mode: "edge", label: "Edge", color: "text-path-edge", dot: "bg-path-edge" },
]

/**
 * Traffic Paperboat carried for the workspace's machines. "Combined" counts every path; the
 * other sources count relay and edge traffic through those nodes (direct traffic uses none).
 */
function Bandwidth({ usage, source }: { usage: Usage; source: TrafficSource }) {
  const breakdown = trafficBreakdown(usage.totals.by_path, source)
  const up = source === "all" ? usage.totals.upload_bytes : breakdown.upload
  const down = source === "all" ? usage.totals.download_bytes : breakdown.download
  const shown = paths.filter((path) => source === "all" || path.mode !== "direct")

  return (
    <div className="flex flex-1 flex-col gap-3">
      {/* The total, and beside it two even lines: upload/download, then the paths (the chart's
          legend). Arrows and dots share one 12px slot so both lines start on the same edge. */}
      <div className="flex items-center gap-5">
        <Figure>{formatBytes(up + down)}</Figure>
        <div className="flex min-w-0 flex-col gap-1.5 text-xs text-muted-foreground">
          <span className="flex items-center gap-4">
            <Mark icon={<ArrowUpIcon className="size-3" />}>{formatBytes(up)}</Mark>
            <Mark icon={<ArrowDownIcon className="size-3" />}>{formatBytes(down)}</Mark>
          </span>
          {up + down > 0 && (
            <span className="flex flex-wrap items-center gap-x-4 gap-y-1.5">
              {shown.map((path) => (
                <Mark key={path.mode} icon={<span className={cn("size-1.5 rounded-full", path.dot)} />}>
                  {path.label} <span className="text-foreground">{formatBytes(breakdown.byPath[path.mode])}</span>
                </Mark>
              ))}
            </span>
          )}
        </div>
      </div>
      <Trend usage={usage} shown={shown} source={source} />
    </div>
  )
}

function Mark({ icon, children }: { icon: React.ReactNode; children: React.ReactNode }) {
  return (
    <span className="flex items-center gap-1.5">
      <span className="flex size-3 shrink-0 items-center justify-center">{icon}</span>
      <span className="whitespace-nowrap trim-cap">{children}</span>
    </span>
  )
}

/**
 * Traffic over the period (hourly or daily, as the server buckets it), one line per path, bled
 * to the card's edges and bottom so it reads as the card's floor.
 */
function Trend({ usage, shown, source }: { usage: Usage; shown: typeof paths; source: TrafficSource }) {
  const gradientId = useId()
  if (usage.series.length < 2) return null
  const perPoint = usage.series.map((point) => trafficBreakdown(point.by_path, source).byPath)
  const lines = shown.map((path) => ({
    key: path.mode,
    color: path.color,
    values: perPoint.map((point) => point[path.mode]),
  }))
  const peak = Math.max(1, ...lines.flatMap((line) => line.values))
  // Each area fades from its line down to nothing (eased, so it does not band); the lines stay
  // crisp, which is what keeps the chart readable.
  const fade = [
    [0, 0.2],
    [0.25, 0.14],
    [0.5, 0.07],
    [0.75, 0.02],
    [1, 0],
  ] as const
  return (
    <svg
      viewBox="0 0 100 32"
      preserveAspectRatio="none"
      className="-mx-5 mt-auto -mb-5 h-16 w-[calc(100%+2.5rem)] shrink-0"
      aria-hidden
    >
      {lines.map(({ key, color, values }) => {
        const points = values.map((value, index): [number, number] => [
          (index / (values.length - 1)) * 100,
          32 - (value / peak) * 26,
        ])
        const line = smoothPath(points)
        return (
          <g key={key} className={color}>
            <linearGradient id={`${gradientId}-${key}`} x1="0" y1="0" x2="0" y2="1">
              {fade.map(([offset, opacity]) => (
                <stop key={offset} offset={offset} stopColor="currentColor" stopOpacity={opacity} />
              ))}
            </linearGradient>
            <path d={`${line} L100,32 L0,32 Z`} fill={`url(#${gradientId}-${key})`} />
            <path d={line} className="fill-none stroke-current" strokeWidth={1.5} vectorEffect="non-scaling-stroke" />
          </g>
        )
      })}
    </svg>
  )
}

/**
 * A smooth curve through the points using monotone cubic interpolation (Fritsch–Carlson): unlike
 * plain splines it never overshoots, so traffic never dips below zero or peaks above the data.
 */
export function smoothPath(points: [number, number][]): string {
  const n = points.length
  const slopes: number[] = []
  for (let i = 0; i < n - 1; i++) {
    slopes.push((points[i + 1][1] - points[i][1]) / (points[i + 1][0] - points[i][0]))
  }
  const tangents = points.map((_, i) => {
    if (i === 0) return slopes[0]
    if (i === n - 1) return slopes[n - 2]
    return slopes[i - 1] * slopes[i] <= 0 ? 0 : (slopes[i - 1] + slopes[i]) / 2
  })
  for (let i = 0; i < n - 1; i++) {
    if (slopes[i] === 0) {
      tangents[i] = tangents[i + 1] = 0
      continue
    }
    const a = tangents[i] / slopes[i]
    const b = tangents[i + 1] / slopes[i]
    const sum = a * a + b * b
    if (sum > 9) {
      const t = 3 / Math.sqrt(sum)
      tangents[i] = t * a * slopes[i]
      tangents[i + 1] = t * b * slopes[i]
    }
  }
  let d = `M${points[0][0].toFixed(2)},${points[0][1].toFixed(2)}`
  for (let i = 0; i < n - 1; i++) {
    const [x0, y0] = points[i]
    const [x1, y1] = points[i + 1]
    const h = (x1 - x0) / 3
    d += ` C${(x0 + h).toFixed(2)},${(y0 + tangents[i] * h).toFixed(2)} ${(x1 - h).toFixed(2)},${(y1 - tangents[i + 1] * h).toFixed(2)} ${x1.toFixed(2)},${y1.toFixed(2)}`
  }
  return d
}

/** A compact "value ⌄" menu for the card header, like the period picker. */
function Menu<T extends string>({
  value,
  options,
  onChange,
}: {
  value: T
  options: Record<string, string>
  onChange: (value: T) => void
}) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger className="flex items-center gap-1 rounded-sm outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/30 data-popup-open:text-foreground">
        {options[value]}
        <CaretDownIcon className="size-3" />
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="min-w-44">
        <DropdownMenuRadioGroup value={value} onValueChange={(next) => onChange(next as T)}>
          {Object.entries(options).map(([key, label]) => (
            <DropdownMenuRadioItem key={key} value={key}>
              {label}
            </DropdownMenuRadioItem>
          ))}
        </DropdownMenuRadioGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

function Card({
  label,
  action,
  loading,
  failed,
  className,
  children,
}: {
  label: string
  action?: React.ReactNode
  loading: boolean
  failed: boolean
  className?: string
  children: React.ReactNode
}) {
  return (
    // The row takes the bandwidth card's height (the others stretch to it); every card reserves
    // that height while loading, so nothing moves when data arrives.
    <div className={cn("surface-pattern flex min-h-[11.125rem] flex-col gap-3 p-5", className)}>
      <div className="flex items-center justify-between text-xs text-muted-foreground">
        <span className="font-medium">{label}</span>
        {action}
      </div>
      {loading ? (
        <Skeleton className="h-8 w-24" />
      ) : failed ? (
        <span className="text-sm text-muted-foreground">Unavailable</span>
      ) : (
        children
      )}
    </div>
  )
}

function Figure({ children }: { children: React.ReactNode }) {
  return <span className="font-heading text-3xl font-semibold tracking-tight">{children}</span>
}
