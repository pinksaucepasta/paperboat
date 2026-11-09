"use client"

import { ArrowDownIcon, ArrowUpIcon } from "@phosphor-icons/react"
import { queryOptions } from "@tanstack/react-query"
import Link from "next/link"
import { useId, useState } from "react"
import { api, useApiQuery, useWorkspace, type Machine, type PathMode, type Usage } from "@/lib/api"
import { formatBytes } from "@/lib/format"
import { usageParams, usagePeriods, type UsagePeriod } from "@/lib/usage-period"
import { cn } from "@/lib/utils"
import { useListScope, ViewToggle } from "@/components/list-toolbar"
import { useCurrentMachine } from "@/components/machine-shell"
import { smoothPath } from "@/components/usage-limits"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { LoadError } from "@/components/load-error"
import { TableRowsSkeleton } from "@/components/skeletons"
import { useTimeFormat } from "@/lib/use-time-format"

/*
 * How much traffic Paperboat carried and which way it went: directly between machines, through a
 * relay, or through an edge (tunnels and previews). From `GET /v1/usage`; these are measurements,
 * not a bill.
 */

/** The machine page's Usage tab. */
export function MachineUsage() {
  return <UsagePage machine={useCurrentMachine()} />
}

const paths: { mode: PathMode; label: string; detail: string; bar: string }[] = [
  { mode: "direct", label: "Direct", detail: "Machine to machine", bar: "bg-path-direct" },
  { mode: "relay", label: "Relay", detail: "Through a relay", bar: "bg-path-relay" },
  { mode: "edge", label: "Edge", detail: "Tunnels and previews", bar: "bg-path-edge" },
]

const kindLabels: Record<string, string> = {
  machine: "Machine",
  terminal_session: "Terminal",
  tunnel: "Tunnel",
  preview: "Preview",
  preview_lease: "Preview",
  file_transfer: "Files",
  // Native consumers (server/internal/resourceusage/native.go).
  terminal: "Terminal",
  exec: "Commands",
  ssh: "SSH",
  file: "Files",
  private_http: "Private access",
  private_tcp: "Private access",
  inspector: "Request inspector",
  config_compare: "Config sync",
}

const usageForQuery = (workspace: string, period: UsagePeriod, machineId: string | null) =>
  queryOptions({
    queryKey: ["usage", period, workspace, machineId ?? ""],
    queryFn: ({ signal }) => {
      const params = usageParams(workspace, period)
      if (machineId) params.set("machine_id", machineId)
      return api<Usage>(`usage?${params}`, { signal })
    },
    staleTime: 60_000,
  })

type Split = Record<PathMode, { up: number; down: number }>

/** Totals per path for the whole period, from the server's aggregates. */
function split(usage: Usage): Split {
  const byPath: Split = { direct: { up: 0, down: 0 }, relay: { up: 0, down: 0 }, edge: { up: 0, down: 0 } }
  for (const path of usage.totals.by_path) {
    byPath[path.mode].up += path.upload_bytes
    byPath[path.mode].down += path.download_bytes
  }
  return byPath
}

export function UsagePage({ machine: tabMachine }: { machine?: Machine }) {
  const workspace = useWorkspace()
  const { scope } = useListScope(tabMachine)
  const [period, setPeriod] = useState<UsagePeriod>("7d")
  const usage = useApiQuery({ ...usageForQuery(workspace?.id ?? "", period, scope), enabled: Boolean(workspace) })
  const data = usage.data

  return (
    <section className="flex flex-col gap-6">
      <div className="flex items-center gap-2">
        <ViewToggle
          value={period}
          onChange={setPeriod}
          views={(Object.keys(usagePeriods) as UsagePeriod[]).map((key) => ({
            value: key,
            label: usagePeriods[key].label.replace("Last ", ""),
          }))}
        />
      </div>

      {usage.isError ? (
        <LoadError what="usage" error={usage.error} retry={() => usage.refetch()} />
      ) : !data ? (
        <UsageSkeleton showMachine={!scope} />
      ) : (
        <Loaded usage={data} period={period} showMachine={!scope} />
      )}
    </section>
  )
}

const topResources = 10

function Loaded({ usage, period, showMachine }: { usage: Usage; period: UsagePeriod; showMachine: boolean }) {
  const byPath = split(usage)
  const total = usage.totals.upload_bytes + usage.totals.download_bytes
  const resources = [...usage.resources]
    .map((resource) => ({ resource, total: resource.upload_bytes + resource.download_bytes }))
    .sort((a, b) => b.total - a.total)
  // The biggest few answer "what used my bandwidth"; the long tail opens on request.
  const [all, setAll] = useState(false)
  const shown = all ? resources : resources.slice(0, topResources)

  return (
    <>
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <Stat label="Total">
          <Figure>{formatBytes(total)}</Figure>
          <span className="flex gap-4 text-xs text-muted-foreground">
            <span className="flex items-center gap-1">
              <ArrowUpIcon className="size-3" />
              {formatBytes(usage.totals.upload_bytes)}
            </span>
            <span className="flex items-center gap-1">
              <ArrowDownIcon className="size-3" />
              {formatBytes(usage.totals.download_bytes)}
            </span>
          </span>
        </Stat>
        {paths.map((path) => {
          const bytes = byPath[path.mode].up + byPath[path.mode].down
          return (
            <Stat
              key={path.mode}
              label={
                <span className="flex items-center gap-1.5">
                  <span className={cn("size-1.5 rounded-full", path.bar)} />
                  {path.label}
                </span>
              }
            >
              <Figure>{formatBytes(bytes)}</Figure>
              <span className="text-xs text-muted-foreground">
                {total > 0 ? `${Math.round((bytes / total) * 100)}% · ` : ""}
                {path.detail}
              </span>
            </Stat>
          )
        })}
      </div>

      <Chart usage={usage} period={period} />

      <div className="flex flex-col gap-3">
        <h2 className="text-sm font-medium">{resources.length > topResources ? "Top resources" : "By resource"}</h2>
        <ResourceTable showMachine={showMachine}>
          {shown.map(({ resource, total: bytes }) => (
            <tr
              key={`${resource.resource_kind}:${resource.resource_id}:${resource.resource_route_id ?? ""}:${resource.machine_id}`}
              className="[&>td]:h-14 [&>td]:px-3"
            >
              <td>
                <span className="flex min-w-0 flex-col">
                  <span className="truncate font-medium">{resource.resource_name || resource.resource_id}</span>
                  <span className="text-xs text-muted-foreground">
                    {kindLabels[resource.resource_kind] ?? resource.resource_kind}
                  </span>
                </span>
              </td>
              {showMachine && (
                <td className="hidden truncate md:table-cell">
                  {resource.machine_id ? (
                    <Link
                      href={`/machines/${encodeURIComponent(resource.machine_id)}/usage`}
                      className="underline-offset-4 hover:underline"
                    >
                      {resource.machine_name || resource.machine_id}
                    </Link>
                  ) : (
                    "—"
                  )}
                </td>
              )}
              <td className="hidden lg:table-cell">
                <PathBar resource={resource} />
              </td>
              <td className="hidden text-right text-muted-foreground tabular-nums sm:table-cell">
                {formatBytes(resource.upload_bytes)}
              </td>
              <td className="hidden text-right text-muted-foreground tabular-nums sm:table-cell">
                {formatBytes(resource.download_bytes)}
              </td>
              <td className="text-right tabular-nums">{formatBytes(bytes)}</td>
            </tr>
          ))}
        </ResourceTable>
        {!resources.length && (
          <p className="py-8 text-center text-sm text-muted-foreground">No traffic in this period.</p>
        )}
        {resources.length > topResources && (
          <Button variant="secondary" className="self-start" onClick={() => setAll(!all)}>
            {all ? `Show top ${topResources}` : `Show all ${resources.length}`}
          </Button>
        )}
      </div>

      {/* A footnote below the table, so it doesn't push the page down when the data lands. */}
      {!usage.coverage.native && (
        <p className="text-sm text-muted-foreground">
          Not every machine reports its direct traffic yet, so direct totals may be low.
        </p>
      )}
    </>
  )
}

/** The By resource table: columns and header, around its rows. */
function ResourceTable({ showMachine, children }: { showMachine: boolean; children: React.ReactNode }) {
  return (
    <table className="w-full table-fixed text-sm">
      <colgroup>
        <col />
        {showMachine && <col className="hidden w-[16%] md:table-column" />}
        <col className="hidden w-[20%] lg:table-column" />
        <col className="hidden w-[12%] sm:table-column" />
        <col className="hidden w-[12%] sm:table-column" />
        <col className="w-[12%]" />
      </colgroup>
      <thead className="border-b border-divider text-left text-xs text-muted-foreground">
        <tr className="[&>th]:h-9 [&>th]:px-3 [&>th]:font-normal">
          <th>Resource</th>
          {showMachine && <th className="hidden md:table-cell">Machine</th>}
          <th className="hidden lg:table-cell">Path</th>
          <th className="hidden text-right sm:table-cell">Up</th>
          <th className="hidden text-right sm:table-cell">Down</th>
          <th className="text-right">Total</th>
        </tr>
      </thead>
      <tbody className="divide-y divide-divider border-b border-divider">{children}</tbody>
    </table>
  )
}

/** How one resource's traffic split across direct, relay and edge, as one thin bar. */
function PathBar({ resource }: { resource: Usage["resources"][number] }) {
  const sums = paths.map((path) => ({
    ...path,
    bytes: resource.paths
      .filter((entry) => entry.mode === path.mode)
      .reduce((sum, entry) => sum + entry.upload_bytes + entry.download_bytes, 0),
  }))
  const total = sums.reduce((sum, entry) => sum + entry.bytes, 0)
  if (!total) return <span className="text-muted-foreground">—</span>
  return (
    <div
      className="flex h-1.5 w-full overflow-hidden rounded-full bg-muted"
      title={sums
        .filter((entry) => entry.bytes)
        .map((entry) => `${entry.label} ${formatBytes(entry.bytes)}`)
        .join(" · ")}
    >
      {sums.map((entry) =>
        entry.bytes ? (
          <span key={entry.mode} className={entry.bar} style={{ width: `${(entry.bytes / total) * 100}%` }} />
        ) : null,
      )}
    </div>
  )
}

/** The loaded page's cards, chart panel and table header, with placeholders for the numbers. */
function UsageSkeleton({ showMachine }: { showMachine: boolean }) {
  const line = (className: string) => <Skeleton className={cn("my-0.5 h-3", className)} />
  return (
    <>
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        {["Total", ...paths.map((path) => path.label)].map((label) => (
          <Stat key={label} label={label}>
            <Skeleton className="my-1 h-6 w-24" />
            {line("w-32")}
          </Stat>
        ))}
      </div>
      {/* The chart panel's own rows (same markup, so the same heights), without the chart. */}
      <div className="surface-pattern flex flex-col gap-3 p-5">
        <div className="flex items-baseline justify-between text-xs text-muted-foreground">
          <span className="font-medium">Traffic</span>
        </div>
        <svg viewBox="0 0 100 40" preserveAspectRatio="none" className="h-48 w-full">
          <line x1="0" y1="39" x2="100" y2="39" className="stroke-foreground/10" strokeWidth={1.5} vectorEffect="non-scaling-stroke" />
        </svg>
        <div className="flex justify-between text-xs text-muted-foreground tabular-nums">
          <span>&nbsp;</span>
        </div>
      </div>
      <div className="flex flex-col gap-3">
        <h2 className="text-sm font-medium">By resource</h2>
        <ResourceTable showMachine={showMachine}>
          <TableRowsSkeleton
            rows={5}
            columns={[
              { bar: "w-40", second: "w-16" },
              showMachine && { className: "hidden md:table-cell", bar: "w-20" },
              { className: "hidden lg:table-cell", bar: "w-full" },
              { className: "hidden text-right sm:table-cell", bar: "w-14" },
              { className: "hidden text-right sm:table-cell", bar: "w-14" },
              { className: "text-right", bar: "w-14" },
            ]}
          />
        </ResourceTable>
      </div>
    </>
  )
}

function Stat({ label, children }: { label: React.ReactNode; children: React.ReactNode }) {
  return (
    <div className="surface-pattern flex flex-col gap-2 p-5">
      <span className="text-xs font-medium text-muted-foreground">{label}</span>
      {children}
    </div>
  )
}

function Figure({ children }: { children: React.ReactNode }) {
  return <span className="font-heading text-2xl font-semibold tracking-tight tabular-nums">{children}</span>
}

/** Traffic over the period, hourly or daily as the server buckets it, with the busiest point marked. */
function Chart({ usage, period }: { usage: Usage; period: UsagePeriod }) {
  const gradient = useId()
  const [hover, setHover] = useState<number>()
  const time = useTimeFormat()
  const values = usage.series.map((point) => point.upload_bytes + point.download_bytes)
  if (values.length < 2) return null
  const peak = Math.max(1, ...values)
  const points = values.map((value, index): [number, number] => [
    (index / (values.length - 1)) * 100,
    40 - (value / peak) * 36,
  ])
  const line = smoothPath(points)
  const label = (iso: string) => time.date(iso, period === "24h" ? "hour" : "day")
  const shown = hover ?? values.indexOf(Math.max(...values))
  const ticks = [...new Set([0, Math.floor((values.length - 1) / 2), values.length - 1])]

  return (
    <div className="surface-pattern flex flex-col gap-3 p-5">
      <div className="flex items-baseline justify-between text-xs text-muted-foreground">
        <span className="font-medium">Traffic</span>
        <span>
          {label(usage.series[shown].start)} ·{" "}
          <span className="text-foreground tabular-nums">{formatBytes(values[shown])}</span>
          {hover === undefined && " at the busiest"}
        </span>
      </div>
      <svg
        viewBox="0 0 100 40"
        preserveAspectRatio="none"
        className="h-48 w-full text-primary"
        onMouseLeave={() => setHover(undefined)}
        onMouseMove={(event) => {
          const box = event.currentTarget.getBoundingClientRect()
          setHover(Math.round(((event.clientX - box.left) / box.width) * (values.length - 1)))
        }}
        aria-label="Traffic over time"
        role="img"
      >
        <linearGradient id={gradient} x1="0" y1="0" x2="0" y2="1">
          <stop offset="0" stopColor="currentColor" stopOpacity={0.18} />
          <stop offset="1" stopColor="currentColor" stopOpacity={0} />
        </linearGradient>
        <path d={`${line} L100,40 L0,40 Z`} fill={`url(#${gradient})`} />
        <path d={line} className="fill-none stroke-current" strokeWidth={1.5} vectorEffect="non-scaling-stroke" />
        <line
          x1={points[shown][0]}
          x2={points[shown][0]}
          y1={0}
          y2={40}
          className="stroke-current opacity-30"
          strokeWidth={1}
          vectorEffect="non-scaling-stroke"
        />
      </svg>
      <div className="flex justify-between text-xs text-muted-foreground tabular-nums">
        {ticks.map((index) => (
          <span key={index}>{label(usage.series[index].start)}</span>
        ))}
      </div>
    </div>
  )
}
