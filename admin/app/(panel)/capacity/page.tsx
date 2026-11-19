"use client"

import { ErrorBlock } from "@/components/error-block"
import { Mono } from "@/components/filters"
import { Page, Stat } from "@/components/page"
import { Time } from "@/components/time"
import { Skeleton } from "@/components/ui/skeleton"
import { type Data } from "@/lib/api"
import { useCursorList } from "@/lib/paging"
import { PagerBar } from "@/components/data-table"
import { useMemo } from "react"
import { formatBytes } from "@/lib/format"
import { cn } from "@/lib/utils"

type Capacity = Data<"/v1/admin/infrastructure/capacity">
type Node = Capacity["items"][number]

/** Memory in binary units, the way the warning thresholds are stated. */
const memory = (bytes: number) => (bytes >= 2 ** 30 ? `${(bytes / 2 ** 30).toFixed(1)} GiB` : `${Math.round(bytes / 2 ** 20)} MiB`)
const percent = (n?: number) => (n === undefined ? "—" : `${n.toFixed(n < 10 ? 1 : 0)}%`)
const severityRank = { critical: 0, warning: 1, info: 2 } as const

/** The worst warning a node has, to colour its row. */
function worst(node: Node): "critical" | "warning" | "info" | undefined {
  return node.warnings.map((w) => w.severity).sort((a, b) => severityRank[a] - severityRank[b])[0]
}

export default function CapacityPage() {
  const filters = useMemo(() => ({}), [])
  const capacity = useCursorList<Capacity>("/v1/admin/infrastructure/capacity", filters, "after", 15_000)
  const data = capacity.data
  return (
    <Page title="Capacity">
      {capacity.isError ? (
        <ErrorBlock what="node capacity" error={capacity.error} retry={() => capacity.refetch()} />
      ) : !data ? (
        <Skeleton className="h-64 w-full" />
      ) : (
        <div className="flex flex-col gap-4" data-capacity-ready>
          <div className="grid grid-cols-2 gap-3 md:grid-cols-5">
            <Stat label="Nodes" value={data.total} note={`${data.summary.edges} edges · ${data.summary.relays} relays · ${data.summary.servers} servers`} />
            <Stat label="Need attention" value={data.summary.attention} />
            <Stat label="Throttled workspaces" value={data.summary.throttled_workspaces} note="Edge and relay traffic slowed past fair use; lift one on its workspace page" />
            <Stat label="Monthly allowance per server" value={formatBytes(data.thresholds.monthly_traffic_bytes)} note={`Warns at ${Math.round(data.thresholds.traffic_warn_fraction * 100)}%`} />
            <Stat label="Edge CPU warning" value={`${data.thresholds.edge_cpu_warn_percent}%`} note={`For ${Math.round(data.thresholds.cpu_sustained_seconds / 60)} min · relay memory ${memory(data.thresholds.relay_rss_warn_bytes)}`} />
          </div>
          <div className="overflow-x-auto">
            <table className="w-full min-w-[64rem] border-collapse text-sm" data-table="capacity" data-state={data.items.length === 0 ? "empty" : "ready"}>
              <thead>
                <tr className="border-b border-divider text-left text-xs text-muted-foreground">
                  {["Node", "Where", "Version", "CPU", "Memory", "Connections", "Today", "Month", "Seen"].map((h) => (
                    <th key={h} scope="col" className="h-9 px-3 font-normal">{h}</th>
                  ))}
                </tr>
              </thead>
              <tbody className="divide-y divide-divider">
                {data.items.map((n) => {
                  const level = worst(n)
                  return (
                    <tr key={n.id} data-node={n.id} data-level={level ?? "ok"} className={cn("align-top [&>td]:px-3 [&>td]:py-3", level === "critical" && "bg-destructive/10", level === "warning" && "bg-amber-500/10")}>
                      <td>
                        <div className="flex flex-col gap-1">
                          <span className="flex items-baseline gap-2"><span className="font-medium">{n.id}</span><span className="text-xs text-muted-foreground">{n.kind}</span></span>
                          {n.warnings.map((w) => (
                            <span key={w.code} data-warning={w.code} className={cn("text-xs", w.severity === "critical" ? "text-destructive" : w.severity === "warning" ? "text-amber-600 dark:text-amber-400" : "text-muted-foreground")}>
                              {w.message}
                            </span>
                          ))}
                        </div>
                      </td>
                      <td>{[n.site, n.region].filter(Boolean).join(" · ") || "—"}</td>
                      <td>{n.version ? <Mono>{n.version}</Mono> : "—"}</td>
                      <td className="tabular-nums">{percent(n.cpu_percent)} / {percent(n.cpu_sustained_percent)}</td>
                      <td className="tabular-nums">{n.rss_bytes === undefined ? "—" : memory(n.rss_bytes)}</td>
                      <td className="tabular-nums">{n.connections === undefined ? "—" : `${n.connections}${n.connection_limit ? ` / ${n.connection_limit}` : ""}`}</td>
                      <td className="tabular-nums">{n.bytes_today === undefined ? "—" : formatBytes(n.bytes_today)}</td>
                      <td className="tabular-nums">
                        {n.bytes_month === undefined || n.kind === "server" ? (
                          "—"
                        ) : (
                          <span className="flex flex-col gap-1">
                            <span>{formatBytes(n.bytes_month)} <span className="text-xs text-muted-foreground">({Math.round((n.allowance_used ?? 0) * 100)}%)</span></span>
                            <span className="h-1.5 w-28 overflow-hidden rounded-full bg-muted" aria-hidden>
                              <span className={cn("block h-full", (n.allowance_used ?? 0) >= 1 ? "bg-destructive" : (n.allowance_used ?? 0) >= data.thresholds.traffic_warn_fraction ? "bg-amber-500" : "bg-primary/70")} style={{ width: `${Math.min(100, (n.allowance_used ?? 0) * 100)}%` }} />
                            </span>
                          </span>
                        )}
                      </td>
                      <td>{n.last_heartbeat_at ? <Time iso={n.last_heartbeat_at} relative /> : "never"}</td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </div>
          <PagerBar page={capacity.page} count={data.items.length} total={data.total} hasNext={capacity.hasNext} go={capacity.go} />
          {data.items.length === 0 && <p className="py-12 text-center text-sm text-muted-foreground">No server has reported yet.</p>}
          <p className="text-xs text-muted-foreground">Observed <Time iso={data.observed_at} relative /> · days in UTC, month from {data.month_start}</p>
        </div>
      )}
    </Page>
  )
}
