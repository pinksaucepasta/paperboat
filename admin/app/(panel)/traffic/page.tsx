"use client"

import { useMemo, useState } from "react"
import { Toggle } from "@/components/analytics"
import { PagerBar } from "@/components/data-table"
import { ErrorBlock } from "@/components/error-block"
import { Mono } from "@/components/filters"
import { Page, Panel, Stat } from "@/components/page"
import { Skeleton } from "@/components/ui/skeleton"
import { useApiQuery, type Data } from "@/lib/api"
import { formatBytes } from "@/lib/format"
import { useCursorList } from "@/lib/paging"
import { useTimeFormat } from "@/lib/use-time-format"
import { cn } from "@/lib/utils"

type Traffic = Data<"/v1/admin/traffic">
type Total = Traffic["totals"][number]
type NodePage = Data<"/v1/admin/traffic/nodes">
type Period = "today" | "7d" | "30d" | "month"

const categories: Record<string, { label: string; color: string; source: string }> = {
  direct: { label: "Direct", color: "bg-emerald-500", source: "machines" },
  relay_managed: { label: "Our relays", color: "bg-sky-500", source: "nodes" },
  relay_self_hosted: { label: "Self-hosted relays", color: "bg-sky-500/40", source: "nodes" },
  edge_managed: { label: "Our edges", color: "bg-violet-500", source: "nodes" },
  edge_self_hosted: { label: "Self-hosted edges", color: "bg-violet-500/40", source: "nodes" },
}
const sum = (t: { upload_bytes: number; download_bytes: number }) => t.upload_bytes + t.download_bytes

/** Whole when every expected source reported, otherwise a labelled partial figure with its coverage. */
function Coverage({ total }: { total: Total }) {
  const { label, source } = categories[total.category]
  if (total.coverage_percent == null) return <span data-coverage={total.category}>No {source} expected</span>
  const pct = Math.round(total.coverage_percent)
  return (
    <span data-coverage={total.category} className={cn(pct < 100 && "text-amber-600 dark:text-amber-400")}>
      {pct < 100 ? "Partial · " : ""}{total.reporting} of {total.expected} {source} reporting ({pct}%)
      <span className="sr-only"> for {label}</span>
    </span>
  )
}

/** One bar per bucket, stacked by path, scaled to the busiest bucket. */
function Series({ data }: { data: Traffic }) {
  const buckets = useMemo(() => {
    const by = new Map<string, Record<string, number>>()
    for (const p of data.series) by.set(p.at, { ...by.get(p.at), [p.category]: p.upload_bytes + p.download_bytes })
    return [...by].map(([at, parts]) => ({ at, parts, total: Object.values(parts).reduce((a, b) => a + b, 0) }))
  }, [data.series])
  const peak = Math.max(1, ...buckets.map((b) => b.total))
  const time = useTimeFormat()
  // Buckets are UTC hours and UTC days; each is labelled by its start in the viewer's own zone, zone named.
  const name = (at: string) => (data.grain === "hour" ? time.dateTime(at) : `UTC day from ${time.dateTime(at)}`)
  if (buckets.length === 0) return <p className="py-8 text-center text-sm text-muted-foreground" data-series="empty">No traffic recorded in this period.</p>
  return (
    <ul role="img" data-series="ready" aria-label={`Traffic per ${data.grain}`} className="flex h-40 items-end gap-0.5 border-b border-divider">
      {buckets.map((b) => (
        <li key={b.at} title={`${name(b.at)}: ${formatBytes(b.total)}`} className="flex h-full min-w-0 flex-1 flex-col-reverse" >
          <div className="flex flex-col-reverse overflow-hidden rounded-t-sm" style={{ height: `${Math.max(2, (b.total / peak) * 100)}%` }}>
            {Object.keys(categories).map((c) => b.parts[c] ? <div key={c} className={categories[c].color} style={{ flexGrow: b.parts[c] }} /> : null)}
          </div>
        </li>
      ))}
    </ul>
  )
}

function SplitBar({ totals }: { totals: Total[] }) {
  const all = totals.reduce((a, t) => a + sum(t), 0)
  return (
    <div className="flex flex-col gap-2" data-split>
      <div className="flex h-3 overflow-hidden rounded-full bg-muted" role="img" aria-label="Share of traffic by path">
        {all > 0 && totals.map((t) => sum(t) > 0 && <div key={t.category} className={categories[t.category].color} style={{ width: `${(sum(t) / all) * 100}%` }} title={`${categories[t.category].label}: ${Math.round((sum(t) / all) * 100)}%`} />)}
      </div>
      <ul className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground">
        {totals.map((t) => (
          <li key={t.category} className="flex items-center gap-1.5"><span className={cn("size-2 rounded-full", categories[t.category].color)} />{categories[t.category].label} {all > 0 ? `${Math.round((sum(t) / all) * 100)}%` : "—"}</li>
        ))}
      </ul>
    </div>
  )
}

function Nodes({ kind, period }: { kind: "relay" | "edge"; period: Period }) {
  const filters = useMemo(() => ({ period, kind }), [period, kind])
  const list = useCursorList<NodePage>("/v1/admin/traffic/nodes", filters, "after", 30_000)
  const data = list.data
  return (
    <Panel title={kind === "relay" ? "Our relays" : "Our edges"}>
      {list.isError ? (
        <ErrorBlock what={`${kind} traffic`} error={list.error} retry={() => list.refetch()} />
      ) : !data ? (
        <Skeleton className="h-32 w-full" />
      ) : (
        <>
          <div className="overflow-x-auto">
            <table className="w-full min-w-[32rem] border-collapse text-sm" data-table={`traffic-${kind}`} data-state={data.items.length === 0 ? "empty" : "ready"}>
              <thead>
                <tr className="border-b border-divider text-left text-xs text-muted-foreground">
                  {["Node", "Region", "Received", "Sent", "Total"].map((h) => <th key={h} scope="col" className="h-9 px-3 font-normal">{h}</th>)}
                </tr>
              </thead>
              <tbody className="divide-y divide-divider">
                {data.items.map((n) => (
                  <tr key={n.node_id} data-node={n.node_id} className="[&>td]:px-3 [&>td]:py-2.5">
                    <td><Mono>{n.node_id}</Mono>{!n.reporting && <span className="ml-2 text-xs text-amber-600 dark:text-amber-400">no report in this period</span>}</td>
                    <td>{n.region || "—"}</td>
                    <td className="tabular-nums">{formatBytes(n.upload_bytes)}</td>
                    <td className="tabular-nums">{formatBytes(n.download_bytes)}</td>
                    <td className="tabular-nums">{formatBytes(n.upload_bytes + n.download_bytes)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {data.items.length === 0 && <p className="py-6 text-center text-sm text-muted-foreground">No {kind}s are registered.</p>}
          <PagerBar page={list.page} count={data.items.length} hasNext={list.hasNext} go={list.go} />
        </>
      )}
    </Panel>
  )
}

export default function TrafficPage() {
  const [period, setPeriod] = useState<Period>("today")
  const traffic = useApiQuery<Traffic>(["traffic", period], `/v1/admin/traffic?period=${period}`, 30_000)
  const data = traffic.data
  return (
    <Page title="Traffic">
      <div className="flex justify-end">
        <Toggle values={[["today", "Today"], ["7d", "7 days"], ["30d", "30 days"], ["month", "This month"]]} value={period} onChange={setPeriod} />
      </div>
      {traffic.isError ? (
        <ErrorBlock what="traffic" error={traffic.error} retry={() => traffic.refetch()} />
      ) : !data ? (
        <Skeleton className="h-64 w-full" />
      ) : (
        <div className="flex flex-col gap-6" data-traffic-ready>
          <div className="grid grid-cols-2 gap-3 md:grid-cols-5">
            {data.totals.map((t) => (
              <Stat key={t.category} label={categories[t.category].label} value={formatBytes(sum(t))} note={<span className="flex flex-col gap-0.5"><span>↑ {formatBytes(t.upload_bytes)} · ↓ {formatBytes(t.download_bytes)}</span><Coverage total={t} /></span>} />
            ))}
          </div>
          <SplitBar totals={data.totals} />
          <Panel title={data.grain === "hour" ? "Per hour" : "Per UTC day"}><Series data={data} /></Panel>
          <Nodes kind="relay" period={period} />
          <Nodes kind="edge" period={period} />
          <p className="text-xs text-muted-foreground">
            Direct is what each host machine reports sending and receiving, so it is a lower bound: machines that do not report are missing. Relays and edges report what they forwarded themselves. Days and months are UTC days; times are shown in your time zone; counts only, no addresses or content.
          </p>
        </div>
      )}
    </Page>
  )
}
