"use client"

import { useMemo, useState } from "react"
import { DataTable } from "@/components/data-table"
import { ErrorBlock } from "@/components/error-block"
import { Filters, Mono, SearchBox, StatusBadge } from "@/components/filters"
import { Panel, Stat } from "@/components/page"
import { Time } from "@/components/time"
import { Skeleton } from "@/components/ui/skeleton"
import { useApiQuery, type Data } from "@/lib/api"
import { formatPlatform, formatUnseen } from "@/lib/format"
import { useCursorList } from "@/lib/paging"
import { useTimeFormat } from "@/lib/use-time-format"
import { cn } from "@/lib/utils"

type Versions = Data<"/v1/admin/fleet/versions">
type Rollouts = Data<"/v1/admin/fleet/rollouts">
type Rollout = Rollouts["rollouts"][number]
type Offline = Data<"/v1/admin/fleet/offline">

const indian = new Intl.NumberFormat("en-IN")
const n = (v: number) => indian.format(v)
const pct = (part: number, whole: number) => (whole === 0 ? "—" : `${((part / whole) * 100).toFixed(part === whole || part === 0 ? 0 : 1)}%`)
const reason = (code: string) => code.replaceAll("_", " ") || "no reason recorded"

function Cells({ head, rows, empty, tag }: { head: string[]; rows: React.ReactNode[][]; empty: string; tag: string }) {
  return (
    <div className="overflow-x-auto" data-table={tag} data-state={rows.length === 0 ? "empty" : "ready"}>
      <table className="w-full min-w-[32rem] border-collapse text-sm">
        <thead>
          <tr className="border-b border-divider text-left text-xs text-muted-foreground">
            {head.map((h, i) => (
              <th key={h} scope="col" className={cn("h-9 px-3 font-normal", i > 1 && "text-right")}>{h}</th>
            ))}
          </tr>
        </thead>
        <tbody className="divide-y divide-divider">
          {rows.map((r, i) => (
            <tr key={i} className="hover:bg-muted/40 [&>td]:h-11 [&>td]:px-3">
              {r.map((c, j) => (
                <td key={j} className={cn("tabular-nums", j > 1 && "text-right")}>{c}</td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
      {rows.length === 0 && <p className="py-8 text-center text-sm text-muted-foreground">{empty}</p>}
    </div>
  )
}

export function FleetVersions() {
  const q = useApiQuery<Versions>(["fleet-versions"], "/v1/admin/fleet/versions", 60_000)
  if (q.isError) return <ErrorBlock what="the fleet versions" error={q.error} retry={() => q.refetch()} />
  const d = q.data
  if (!d) return <Skeleton className="h-64 w-full" />
  return (
    <div className="flex flex-col gap-8" data-fleet-versions-ready>
      <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
        <Stat label="Machines" value={n(d.machines)} note={`${n(d.online)} online`} />
        <Stat label="Users" value={n(d.users)} note="Owners of at least one machine" />
        <Stat label="Teams" value={n(d.teams)} note="Teams that own a machine" />
        <Stat label="Oldest version online" value={d.oldest_online_version ? <Mono>{d.oldest_online_version}</Mono> : "—"} note="Release versions only; dev builds are not ordered" />
      </div>
      <div className="grid grid-cols-2 gap-3 md:grid-cols-4" data-fleet-extra>
        <Stat label="Non-official builds" value={n(d.non_official.machines)} note={`${n(d.non_official.online)} online · ${n(d.non_official.versions)} ${d.non_official.versions === 1 ? "version" : "versions"} (dev and custom builds)`} />
        <Stat label="Self-hosted relays" value={n(d.self_hosted.relays.registered)} note={`${n(d.self_hosted.relays.online)} online · ${n(d.self_hosted.relays.reporting)} reporting traffic`} />
        <Stat label="Self-hosted edges" value={n(d.self_hosted.edges.registered)} note={`${n(d.self_hosted.edges.online)} online · ${n(d.self_hosted.edges.reporting)} reporting traffic`} />
        <Stat label="Teams with self-hosted nodes" value={n(d.self_hosted.teams)} note={`Plus ${n(d.self_hosted.accounts)} personal ${d.self_hosted.accounts === 1 ? "account" : "accounts"}. Customer nodes only`} />
      </div>
      <Panel title="By OS">
        <Cells
          tag="fleet-platforms"
          empty="No machines."
          head={["OS", "Oldest online", "Machines", "Online", "Users", "Teams"]}
          rows={d.platforms.map((p) => [formatPlatform(p.platform), p.oldest_online_version ? <Mono key="o">{p.oldest_online_version}</Mono> : "—", n(p.machines), n(p.online), n(p.users), n(p.teams)])}
        />
      </Panel>
      <Panel title="By version">
        <Cells
          tag="fleet-versions"
          empty="No machine has reported a version yet."
          head={["Version", "OS", "Machines", "Online", "Users", "Teams"]}
          rows={d.versions.map((v) => [<Mono key="v">{v.version}</Mono>, formatPlatform(v.platform), n(v.machines), n(v.online), n(v.users), n(v.teams)])}
        />
      </Panel>
      <Panel title="Update failures">
        <Cells
          tag="fleet-failures"
          empty="No machine reports a failed or rolled back update."
          head={["Update to", "Outcome", "Machines", "Reason"]}
          rows={d.failures.map((f) => [<Mono key="t">{f.target_version || "—"}</Mono>, <StatusBadge key="s" state={f.state === "failed" ? "failed" : "rolled back"} />, n(f.machines), <span key="r" className="text-left">{reason(f.error_code)}</span>])}
        />
      </Panel>
    </div>
  )
}

/** Updated share per day since the rollout began: one line, direct labels, 0 and 100% rules. */
function RolloutChart({ rollout }: { rollout: Rollout }) {
  const days = rollout.series
  if (days.length < 2) return <p className="text-sm text-muted-foreground" data-rollout-chart-empty>The chart needs two days of snapshots; the first one is written within ten minutes of the server starting.</p>
  const w = 640, h = 160, left = 40, right = 12, top = 12, bottom = 24
  const x = (i: number) => left + (i * (w - left - right)) / (days.length - 1)
  const y = (share: number) => top + (1 - share) * (h - top - bottom)
  const share = days.map((d) => (d.total === 0 ? 0 : d.updated / d.total))
  const last = days[days.length - 1]
  return (
    <svg viewBox={`0 0 ${w} ${h}`} role="img" className="w-full max-w-2xl" data-rollout-chart={rollout.version}
      aria-label={`Share of machines on ${rollout.version} or newer, ${days[0].day} to ${last.day}: ${pct(last.updated, last.total)} on the last day`}>
      {[0, 0.5, 1].map((t) => (
        <g key={t}>
          <line x1={left} x2={w - right} y1={y(t)} y2={y(t)} className="stroke-divider" strokeWidth={1} />
          <text x={left - 6} y={y(t)} textAnchor="end" dominantBaseline="middle" className="fill-muted-foreground text-[10px]">{t * 100}%</text>
        </g>
      ))}
      <polyline fill="none" strokeWidth={2} strokeLinejoin="round" className="stroke-foreground" points={share.map((s, i) => `${x(i)},${y(s)}`).join(" ")} />
      <circle cx={x(days.length - 1)} cy={y(share[share.length - 1])} r={3} className="fill-foreground" />
      <text x={left} y={h - 6} className="fill-muted-foreground text-[10px]">{days[0].day}</text>
      <text x={w - right} y={h - 6} textAnchor="end" className="fill-muted-foreground text-[10px]">{last.day}</text>
    </svg>
  )
}

const decision: Record<string, string> = { promote: "Rolling out", pause: "Paused", quarantine: "Quarantined", revoke: "Revoked" }

function RolloutCard({ r }: { r: Rollout }) {
  const reachable = r.updated + r.older
  const stillOnline = r.older - r.offline_since_start - r.offline_now
  return (
    <Panel
      title={`Version ${r.version}`}
      action={
        <span className="flex flex-wrap items-center gap-3 text-xs text-muted-foreground">
          <span>{decision[r.action] ?? r.action}{r.action === "promote" ? ` to ${r.rollout_percentage}%` : ""}</span>
          <span>{r.platforms.map(formatPlatform).join(", ")}</span>
          <span>Started <Time iso={r.started_at} ist /></span>
        </span>
      }
    >
      <div className="flex flex-col gap-4" data-rollout={r.version}>
        <div className="grid grid-cols-2 gap-3 md:grid-cols-4">
          <Stat label="Updated" value={pct(r.updated, reachable)} note={`${n(r.updated)} of ${n(reachable)} machines`} />
          <Stat label="Still on an older version" value={n(r.older)} note={`${n(Math.max(stillOnline, 0))} online now`} />
          <Stat label="Failed or rolled back" value={n(r.failed + r.rolled_back)} note={`${n(r.failed)} failed · ${n(r.rolled_back)} rolled back`} />
          <Stat label="Offline the whole time" value={n(r.offline_since_start)} note="Last seen before the rollout began, so they could not update" />
        </div>
        <p className="text-xs text-muted-foreground">
          {n(r.offline_now)} more are offline now but were seen since the start.
          {r.other > 0 && ` ${n(r.other)} machines run dev builds or report no version and are not counted.`}
        </p>
        <RolloutChart rollout={r} />
        {r.failures.length > 0 && (
          <Cells tag="rollout-failures" empty="" head={["Outcome", "Reason", "Machines"]} rows={r.failures.map((f) => [<StatusBadge key="s" state={f.state === "failed" ? "failed" : "rolled back"} />, reason(f.error_code), n(f.machines)])} />
        )}
      </div>
    </Panel>
  )
}

export function FleetRollouts() {
  const q = useApiQuery<Rollouts>(["fleet-rollouts"], "/v1/admin/fleet/rollouts", 60_000)
  if (q.isError) return <ErrorBlock what="the rollouts" error={q.error} retry={() => q.refetch()} />
  if (!q.data) return <Skeleton className="h-64 w-full" />
  if (q.data.rollouts.length === 0) return <p className="py-12 text-center text-sm text-muted-foreground" data-table="fleet-rollouts" data-state="empty">No release has been promoted yet.</p>
  return (
    <div className="flex flex-col gap-10" data-fleet-rollouts-ready>
      {q.data.rollouts.map((r) => <RolloutCard key={r.version} r={r} />)}
    </div>
  )
}

const buckets: [string, string][] = [["under_1_day", "Under 1 day"], ["1_to_7_days", "1–7 days"], ["7_to_30_days", "7–30 days"], ["over_30_days", "Over 30 days"], ["never_seen", "Never seen"]]

/** The exact time since last seen; before the clock is known (first render) the bucket's name. */
function UnseenFor({ m }: { m: Offline["items"][number] }) {
  const { now } = useTimeFormat(60_000)
  if (!m.last_seen_at) return "Never seen"
  return formatUnseen(m.last_seen_at, now) || (buckets.find(([key]) => key === m.bucket)?.[1] ?? m.bucket)
}

export function FleetOffline() {
  const [q, setQ] = useState("")
  const [bucket, setBucket] = useState("")
  const [platform, setPlatform] = useState("")
  const filters = useMemo(() => ({ q, bucket, platform }), [q, bucket, platform])
  const list = useCursorList<Offline>("/v1/admin/fleet/offline", filters)
  const d = list.data
  return (
    <div className="flex flex-col gap-6" data-fleet-offline-ready={d ? "" : undefined}>
      <div className="grid grid-cols-2 gap-3 md:grid-cols-6">
        <Stat label="Offline now" value={d ? n(d.offline) : "—"} note={d ? `of ${n(d.fleet_machines)} machines` : undefined} />
        {buckets.map(([key, label]) => {
          const count = d?.buckets.find((b) => b.bucket === key)?.machines
          return (
            <button key={key} type="button" onClick={() => setBucket(bucket === key ? "" : key)} aria-pressed={bucket === key} data-bucket={key}
              className={cn("text-left outline-none focus-visible:ring-2 focus-visible:ring-ring/30", bucket === key && "ring-2 ring-foreground/60")}>
              <Stat label={label} value={count === undefined ? "—" : n(count)} />
            </button>
          )
        })}
      </div>
      <div className="flex flex-wrap items-center gap-2">
        <SearchBox label="Search offline machines" placeholder="Id, name or owner" value={q} onChange={setQ} />
        <Filters items={[{ label: "OS", value: platform, onChange: setPlatform, options: [["linux", "Linux"], ["darwin", "macOS"], ["windows", "Windows"]] }]} />
      </div>
      <DataTable
        what="fleet-offline"
        list={list}
        rows={(p) => p.items}
        rowKey={(m) => m.id}
        empty="No offline machine matches."
        columns={[
          { header: "Machine", cell: (m) => <span className="flex flex-col"><span className="font-medium">{m.alias}</span><Mono>{m.id}</Mono></span> },
          { header: "OS", cell: (m) => `${formatPlatform(m.platform)} · ${m.architecture}` },
          { header: "Last version", cell: (m) => <Mono>{m.version}</Mono> },
          { header: "Last seen", cell: (m) => (m.last_seen_at ? <Time iso={m.last_seen_at} ist /> : "Never") },
          { header: "Unseen for", cell: (m) => <UnseenFor m={m} /> },
          { header: "Owner", cell: (m) => <Mono>{m.team_id || m.account_id}</Mono> },
        ]}
      />
    </div>
  )
}
