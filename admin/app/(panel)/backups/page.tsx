"use client"

import { useMemo } from "react"
import { DataTable } from "@/components/data-table"
import { ErrorBlock } from "@/components/error-block"
import { Mono, StatusBadge } from "@/components/filters"
import { Facts, Page, Panel } from "@/components/page"
import { Time } from "@/components/time"
import { Skeleton } from "@/components/ui/skeleton"
import { useApiQuery, type Data } from "@/lib/api"
import { formatBytes } from "@/lib/format"
import { useCursorList } from "@/lib/paging"

type Status = Data<"/v1/admin/backups">
type Runs = Data<"/v1/admin/backups/runs">
type Run = Runs["runs"][number]

// The server's evidence states (backupreport.evidenceState), in words.
const stateWord: Record<string, string> = { current: "Current", stale: "Stale", failed: "Failed", expired: "Expired", unknown: "No reports", not_configured: "Not set up" }
const stateDot = (s: string): "ready" | "failed" | "unknown" => (s === "current" ? "ready" : s === "unknown" || s === "not_configured" ? "unknown" : "failed")

function State({ state }: { state: string }) {
  return (
    <span className="inline-flex items-center gap-2" data-backup-state={state}>
      <span className={`size-1.5 shrink-0 rounded-full ${stateDot(state) === "ready" ? "bg-success" : stateDot(state) === "failed" ? "bg-destructive" : "bg-muted-foreground/35"}`} aria-hidden />
      {stateWord[state] ?? "Unknown"}
    </span>
  )
}

const runText = (r?: Run | null) => (r ? <span key={r.run_id} className="inline-flex flex-wrap items-center gap-x-2"><Mono>{r.run_id}</Mono><span className="text-muted-foreground">{r.outcome === "succeeded" ? "Succeeded" : "Failed"} · <Time iso={r.completed_at} relative /></span></span> : "—")

/** Backup and restore evidence the deployment reports: the current state, then every run. */
export default function BackupsPage() {
  const status = useApiQuery<Status>(["backups"], "/v1/admin/backups", 60_000)
  const filters = useMemo(() => ({}), [])
  const runs = useCursorList<Runs>("/v1/admin/backups/runs", filters)
  const s = status.data
  return (
    <Page title="Backups">
      <Panel title="Status">
        {status.isError ? (
          <ErrorBlock what="the backup status" error={status.error} retry={() => status.refetch()} />
        ) : !s ? (
          <Skeleton className="h-32 w-full" />
        ) : (
          <div data-backup-status>
            <Facts
              rows={[
                ["Backups", <State key="b" state={s.backup_state} />],
                ["Restore test", <State key="r" state={s.restore_state} />],
                ["Latest backup", runText(s.latest_backup)],
                ["Latest restore test", runText(s.latest_restore)],
                ["Last good backup", runText(s.last_successful_backup)],
                ["Restore tested", s.latest_backup_restore_verified ? "The latest backup was restored" : "Not for the latest backup"],
              ]}
            />
          </div>
        )}
      </Panel>
      <Panel title="Runs">
        <DataTable
          what="backup runs"
          list={runs}
          rows={(d) => d.runs}
          rowKey={(r) => r.run_id}
          empty="No backup reports yet."
          columns={[
            { header: "Run", cell: (r) => <Mono>{r.run_id}</Mono> },
            { header: "Kind", cell: (r) => (r.kind === "restore" ? "Restore test" : "Backup") },
            { header: "Outcome", cell: (r) => <StatusBadge state={r.outcome === "succeeded" ? "succeeded" : "failed"} /> },
            { header: "Size", className: "hidden md:table-cell tabular-nums", cell: (r) => (r.kind === "backup" ? formatBytes(r.bytes) : "—") },
            { header: "Restored from", className: "hidden lg:table-cell", cell: (r) => (r.backup_run_id ? <Mono>{r.backup_run_id}</Mono> : "—") },
            { header: "Completed", cell: (r) => <Time iso={r.completed_at} relative /> },
          ]}
        />
      </Panel>
    </Page>
  )
}
